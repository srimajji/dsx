# Repository-Local Git Configuration Compatibility Plan

## Status

Planned. This document defines the problem, implementation scope, and acceptance criteria. It does not claim the changes are implemented. `docs/PRD.md` and `docs/adr/0001-dsx-implementation-architecture.md` remain authoritative.

Decision type: small, reversible compatibility improvement that preserves the existing host Git security boundary.

## 1. Problem

DSX uses host Git to create and verify restrictive source bundles and to fetch or apply workspace results. Repository-local `.git/config` is therefore an input to DSX-managed host Git operations. Some Git configuration can execute commands, load additional configuration, acquire objects through unexpected transports, or otherwise change Git behavior.

DSX currently fails closed by accepting only explicitly reviewed repository-local Git configuration. This policy correctly blocks command-bearing configuration, but its branch-metadata allowlist is too narrow for repositories used with common editor and GitHub tooling.

For `/Volumes/Dev/work/course-intelligence-agency`, VS Code and GitHub tooling added inert branch metadata such as:

```text
branch.main.vscode-merge-base=origin/main
branch.feat/core-19-syllabus-fetch-generate-flow.vscode-merge-base=origin/main
branch.feat/core-19-syllabus-fetch-generate-flow.github-pr-owner-number=StuDocu#course-intelligence-agency#11
```

Workspace creation then failed with only the first unsupported key:

```text
dsx workspace create: repository-local Git configuration "branch.main.vscode-merge-base" is not allowlisted
```

This creates two immediate problems:

1. Harmless IDE metadata blocks workspace creation even though it does not alter DSX's Git transfer behavior.
2. DSX reports one unsupported key at a time, forcing users to remove a key and retry repeatedly before discovering the complete incompatibility set.

The current workaround is to delete the metadata from `.git/config`. That is unnecessary friction and the tools may add it again.

## 2. Goal

Improve compatibility and diagnostics without weakening the Git security model:

- allow the reviewed inert branch metadata keys `branch.*.vscode-merge-base` and `branch.*.github-pr-owner-number`;
- report all unsupported repository-local Git configuration keys in one deterministic error;
- document why command-bearing and object-acquiring Git configuration remains blocked.

## 3. Non-goals

This change does not:

- allow arbitrary `branch.*` configuration;
- replace the allowlist with a denylist;
- expose rejected configuration values in errors or logs;
- disable protected host Git environment handling;
- follow repository-local `include` or `includeIf` directives;
- change source bundles, private guest clones, workspace updates, result fetch, or guarded apply behavior;
- introduce direct host workspace mounts or Docker Sandbox-style filesystem synchronization;
- change the PRD or ADR security architecture.

## 4. Required behavior

### 4.1 Accepted inert metadata

DSX accepts these key shapes for any branch subsection:

```text
branch.<branch>.vscode-merge-base
branch.<branch>.github-pr-owner-number
```

Values must pass the existing safe scalar validation. The implementation must add the two exact leaf names to the reviewed branch-key allowlist; it must not accept arbitrary keys with similar prefixes.

Examples that should be accepted:

```text
branch.main.vscode-merge-base=origin/main
branch.feat/core-19.vscode-merge-base=origin/main
branch.feat/core-19.github-pr-owner-number=StuDocu#course-intelligence-agency#11
```

Examples that must remain rejected:

```text
branch.main.vscode-command=/tmp/run-me
branch.main.github-pr-command=/tmp/run-me
branch.main.unreviewed=value
```

### 4.2 Aggregated unsupported-key error

DSX inspects the complete bounded local configuration and returns one error containing every unsupported key.

Required error properties:

- keys are normalized to lowercase;
- duplicate keys appear once;
- keys are sorted lexicographically;
- values are never included;
- the result remains bounded by the existing Git output and configuration-file limits;
- validation performs no repository mutation.

Proposed stable message:

```text
repository-local Git configuration keys are not allowlisted: "core.fsmonitor", "credential.helper", "merge.payload.driver"
```

Use the same plural form for one or multiple keys to avoid separate message contracts.

### 4.3 Includes remain blocked

The raw `.git/config` pre-scan remains necessary because DSX must not follow repository-controlled includes while discovering configuration.

The implementation must continue invoking:

```text
git config --local --no-includes --null --list
```

Detected `include` and `includeIf` configuration must be added to the aggregated rejected-key set without reading the included file. Unsafe file metadata, concurrent file replacement, read failure, or Git parse failure remains an immediate structural error because DSX cannot claim a complete key inventory in those cases.

### 4.4 Command-bearing configuration remains blocked

At minimum, existing rejection coverage remains for:

```text
core.fsmonitor
core.alternateRefsCommand
credential.helper
filter.*.process
diff.*.command
merge.*.driver
gc.recentObjectsHook
include.path
includeIf.*.path
```

Remote URLs that select command transports, such as `ext::`, also remain rejected even though the key `remote.*.url` is ordinarily supported.

## 5. Implementation plan

### 5.1 Extend the branch metadata allowlist

**File:** `internal/gitx/service.go`  
**Function:** `allowlistedLocalGitConfig`

In the `branch` key handling, add these exact leaf names:

```text
vscode-merge-base
github-pr-owner-number
```

Validate both with `safeGitConfigScalar`, matching other inert branch metadata. Do not introduce a new wildcard or extension-specific abstraction; two explicit leaf cases keep the reviewed authority visible.

### 5.2 Collect unsupported keys

**File:** `internal/gitx/service.go`  
**Functions:** `validateLocalConfiguration`, `rejectRepositoryIncludeConfig`

Refactor validation as follows:

1. Safely pre-scan `.git/config` for include directives without following them.
2. Run the existing protected `git config --local --no-includes --null --list` command.
3. Inspect every bounded record rather than returning on the first unsupported record.
4. Add unsupported normalized keys to a set.
5. Merge rejected include keys into the same set.
6. Sort the set.
7. Return one value-free error when the set is non-empty.

Prefer a small helper that formats the sorted rejected-key set. Do not introduce a general validation framework or exported error type unless an existing caller requires structured inspection; current callers render the error directly.

Structural safety errors remain distinct from policy rejection. Examples include a non-regular `.git/config`, configuration replacement during inspection, oversized configuration, unreadable data, or malformed Git output.

### 5.3 Preserve command isolation

Do not change:

- the canonical Git executable selected by `gitx.Service`;
- the protected environment used for host Git;
- `GIT_CONFIG_NOSYSTEM` and controlled global configuration behavior;
- structured Git argv execution;
- output limits;
- repository identity revalidation;
- bundle verification and cleanup;
- transactional apply validation.

## 6. Test plan

**File:** `internal/gitx/service_test.go`  
**Primary test:** `TestHostGitUsesProtectedConfigurationAndRejectsUnallowlistedLocalConfig`

### 6.1 Accepted metadata

Extend the ordinary clone configuration case with:

```text
branch.main.vscode-merge-base=origin/main
branch.main.github-pr-owner-number=StuDocu#repository#11
branch.feat/core-19.vscode-merge-base=origin/main
branch.feat/core-19.github-pr-owner-number=StuDocu#repository#11
```

Assert that `PrepareSource` succeeds and its bundle verifies.

Add negative boundaries proving that:

- similarly named unreviewed branch keys remain rejected;
- unsafe scalar values remain rejected according to `safeGitConfigScalar`;
- branch names containing `/` and `.` do not break key parsing.

### 6.2 Aggregated diagnostics

Create one repository-local configuration containing multiple rejected entries, including a duplicate and an include directive:

```text
core.fsmonitor
credential.helper
filter.payload.process
include.path
credential.helper
```

Assert that:

- one error reports every distinct rejected key;
- keys are normalized and sorted;
- the duplicate appears once;
- no configured value appears in the error;
- none of the referenced executables run;
- no source artifact or host ref mutation occurs.

Update existing exact single-key assertions to the plural stable message. Keep the existing per-key security table so every dangerous key remains independently defended.

### 6.3 Command transport

Keep the existing `ext::` remote transport test. Update only its expected error message if the aggregated formatter changes the single-key output.

## 7. Documentation plan

**File:** `docs/manual/user-guide.md`  
**Section:** `2.3 Repository and mount rules`

Add a concise subsection titled **Repository-local Git configuration** explaining:

- DSX runs host Git for restrictive source and result transfer;
- `.git/config` can change Git behavior and is therefore security-relevant;
- DSX accepts reviewed inert repository, identity, branch, and remote metadata;
- VS Code merge-base and GitHub PR-number branch metadata are supported;
- command-bearing configuration, includes, credential helpers, filters, drivers, hooks, and command transports remain blocked;
- one rejection reports all unsupported keys without exposing their values;
- remediation is to remove an unsupported repository-local key or move an appropriate user preference to global configuration, not to disable DSX validation.

No PRD or ADR update is required because the underlying trust boundary and product behavior remain unchanged.

## 8. Verification

Format and run focused tests:

```console
gofmt -w internal/gitx/service.go internal/gitx/service_test.go

go test ./internal/gitx \
  -run '^TestHostGitUsesProtectedConfigurationAndRejectsUnallowlistedLocalConfig$' \
  -count=1

go test ./internal/gitx -count=1
```

Then verify against an isolated temporary repository containing both supported metadata keys and multiple blocked command-bearing keys. The supported case must create and verify a source artifact; the blocked case must return one sorted, deduplicated, value-free error and execute no configured command.

After focused verification, run the repository suite:

```console
go test ./...
```

This change affects workspace creation behavior. After implementation, build the host binary and run the required safe isolated-project Apple runtime creation-and-attach smoke test before claiming the change complete.

## 9. Acceptance criteria

1. `branch.*.vscode-merge-base` no longer blocks workspace source preparation when its value is a safe scalar.
2. `branch.*.github-pr-owner-number` no longer blocks workspace source preparation when its value is a safe scalar.
3. Arbitrary or similarly named branch keys remain rejected.
4. All unsupported repository-local keys are reported in one deterministic error.
5. Rejected keys are lowercased, deduplicated, and sorted.
6. Rejected values never appear in errors or logs.
7. Include directives are detected without being followed and appear in the aggregate rejection.
8. Existing command-bearing configuration and command-transport tests continue to prove that configured executables do not run.
9. Source creation, bundle verification, result fetch, and guarded apply retain their existing protected Git environment and repository identity checks.
10. The manual explains both the supported IDE metadata and the reason command-bearing Git configuration remains blocked.
