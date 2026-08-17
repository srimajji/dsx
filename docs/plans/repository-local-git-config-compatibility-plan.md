# Repository-Local Git Configuration Compatibility Plan

## Status

Implemented and verified. Focused, race, vet, full-repository, host-build, isolated Apple runtime create-and-attach, include-first rejection, aggregate rejection, restoration, and exact cleanup checks passed on 2026-08-12. `docs/PRD.md` and `docs/adr/0001-dsx-implementation-architecture.md` remain authoritative.

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

- allow the reviewed inert branch metadata keys `branch.*.vscode-merge-base`, `branch.*.github-pr-owner-number`, and `branch.*.gh-merge-base`, and the reviewed inert remote metadata key `remote.*.gh-resolved`;
- detect direct `include.path` and `includeIf.*.path` assignments without Git, report them first, and stop the repository-configuration validation before its Git subprocess or any include-target access;
- after that preflight passes, report all safely listable unsupported repository-local Git configuration keys in one deterministic error with self-service remediation;
- document why command-bearing and object-acquiring Git configuration remains blocked.

The GitHub CLI keys are included in the same review batch because `gh repo set-default` writes `remote.<name>.gh-resolved` and `gh pr create` reads and documents `branch.<branch>.gh-merge-base`; repositories used with `gh` would otherwise fail workspace creation for the same inert-metadata reason immediately after this change ships.

## 3. Non-goals

This change does not:

- allow arbitrary `branch.*` configuration;
- replace the allowlist with a denylist;
- expose rejected configuration values in errors or logs;
- disable protected host Git environment handling;
- follow or read the targets of repository-local `include` or `includeIf` directives;
- change source bundles, private guest clones, workspace updates, result fetch, or guarded apply behavior;
- introduce direct host workspace mounts or Docker Sandbox-style filesystem synchronization;
- change the PRD or ADR security architecture.

## 4. Required behavior

### 4.1 Accepted inert metadata

DSX accepts these key shapes for any branch or remote subsection:

```text
branch.<branch>.vscode-merge-base
branch.<branch>.github-pr-owner-number
branch.<branch>.gh-merge-base
remote.<remote>.gh-resolved
```

Values must pass the existing safe scalar validation (`safeGitConfigScalar` already accepts `#`, which `github-pr-owner-number` values contain). The implementation must add the exact leaf names to the reviewed branch-key and remote-key allowlists; it must not accept arbitrary keys with similar prefixes.

Examples that should be accepted:

```text
branch.main.vscode-merge-base=origin/main
branch.feat/core-19.vscode-merge-base=origin/main
branch.feat/core-19.github-pr-owner-number=StuDocu#course-intelligence-agency#11
branch.feat/core-19.gh-merge-base=main
remote.origin.gh-resolved=base
```

Examples that must remain rejected:

```text
branch.main.vscode-command=/tmp/run-me
branch.main.github-pr-command=/tmp/run-me
branch.main.unreviewed=value
remote.origin.gh-unreviewed=value
```

### 4.2 Aggregated safely listable unsupported-key error

Actual `include.path` and `includeIf.*.path` assignments are the deliberate exception to aggregation. DSX performs the include preflight in section 4.3 first and returns any such direct assignments before invoking Git for that validation. The user removes those assignments and retries. Only after the preflight passes does DSX ask Git for the complete bounded local configuration and return one error containing every safely listable unsupported key.

Required error properties:

- the section (first component) and leaf (last component) are normalized to lowercase; the subsection (for example, the branch name) preserves its original casing because Git subsections are case-sensitive and a lowercased branch name would not match the file or work in a `git config --unset-all` command;
- duplicate keys appear once;
- keys are sorted lexicographically over the normalized form;
- values are never included;
- the result remains bounded by the existing Git output and configuration-file limits;
- validation performs no repository mutation.

Proposed stable message:

```text
repository-local Git configuration keys are not allowlisted: "core.fsmonitor", "credential.helper", "merge.payload.driver"
remove a key with: git config --local --unset-all <key>
```

Use the same plural form for one or multiple keys to avoid separate message contracts. The remediation line makes the rejection self-service without exposing values; keys are not secrets.

Valueless keys (implicit Git booleans such as a bare `bare` under `[core]`) appear in `--null --list` records without a value separator. The implementation treats each such record as an empty value and validates it against the allowlist. Focused tests cover both an allowlisted valueless implicit boolean and an unreviewed valueless key, so this behavior is deterministic rather than incidental.

### 4.3 Includes are blocked before Git

`git config --local --no-includes --null --list` is not a safe include-discovery boundary. Git may resolve repository configuration while setting up any repository command, before the command-level `--no-includes` behavior can protect the listing. DSX must therefore reject direct include path assignments without relying on Git, including when the worktree uses a `.git` file.

During a repository-configuration validation, before DSX starts the Git command associated with that validation, DSX resolves the active repository configuration path using bounded filesystem reads:

1. A physical `.git` directory selects its `config`.
2. A `.git` gitfile must be a non-symlink regular file of at most 64 KiB containing exactly one `gitdir: ` line with an optional LF or CRLF terminator. Its relative target is repository-relative. The resolved target must be a canonical physical directory. This covers linked worktrees and submodules without invoking Git.
3. An optional `<gitdir>/commondir` must, when present, be a non-symlink regular file of at most 64 KiB containing exactly one path line with an optional LF or CRLF terminator. Its relative target is Git-directory-relative. The resolved target must be a canonical physical directory and supplies the active `config`. When `commondir` is absent, DSX race-revalidates that absence and the Git directory supplies the active `config`.

An absent resolved common `config` is a legitimate empty repository-local configuration only after the Git directory, any present `commondir` pointer and target, and the configuration's continued absence have been revalidated. Other metadata, path, and file failures remain structural errors distinct from policy rejection. A missing required `.git` metadata entry or gitfile, a malformed `.git` gitfile or present `commondir` pointer, a missing referenced target, symlinks, oversized or unsafe path data, canonicalization or permission failures, replacement races, and unreadable, non-regular, or oversized configuration fail closed rather than falling through to Git.

When the resolved common `config` exists, DSX performs a bounded stable read of only that file. It strips exactly one leading UTF-8 byte-order mark before scanning the first section header; gitfiles and `commondir` files do not accept a BOM. DSX inspects actual `path` assignments in direct `[include]` and `[includeIf "..."]` sections and never resolves, opens, or reads an include target. Empty include section headers alone are not policy violations. If direct include path assignments exist, DSX returns their normalized keys sorted, deduplicated, and without values, then does not start the Git command for that validation. Other unsupported keys are intentionally absent from this first error. The user removes all include assignments and retries to receive the aggregate safely listable rejection from section 4.2.

These bounded snapshots and revalidations protect against repository-controlled static input and fail closed when a metadata or configuration mutation is observable during the checks. The local host user is trusted. DSX does not claim to defend against that user, or another process acting with that user's authority, concurrently replacing `.git` metadata after a check or while the following Git process starts.

After the include preflight passes, DSX may invoke:

```text
git config --local --no-includes --null --list
```

The flag remains a conservative command option, not the listing-level defense for `.git`-file layouts and not permission to defer include detection to Git.

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

### 4.5 Worktree configuration is rejected before activation

When `extensions.worktreeConfig` is enabled, Git reads `$GIT_DIR/config.worktree` in addition to the common `config`, and `git config --local` inspection would not safely cover that extra file. DSX therefore detects configuration that enables `extensions.worktreeConfig`, including a valueless implicit-true assignment, from the bounded common-config snapshot during filesystem preflight. Direct include path assignments retain the first-error precedence from section 4.3; once none remain, DSX rejects `extensions.worktreeconfig` and does not start the Git command for that validation. For repository-controlled static input, this prevents Git from activating an unscanned `config.worktree`.

Disabled forms do not activate `config.worktree`, but they remain unsupported and proceed to the normal aggregate allowlist rejection after preflight. No form of `extensions.worktreeConfig` is accepted. Any future acceptance would require a separate safe inspection contract for worktree configuration.

## 5. Implementation plan

### 5.1 Extend the branch and remote metadata allowlists

**File:** `internal/gitx/service.go`  
**Function:** `allowlistedLocalGitConfig`

In the `branch` key handling, add these exact leaf names:

```text
vscode-merge-base
github-pr-owner-number
gh-merge-base
```

In the `remote` key handling, add this exact leaf name:

```text
gh-resolved
```

Validate all four with `safeGitConfigScalar`, matching other inert metadata. Do not introduce a new wildcard or extension-specific abstraction; explicit leaf cases keep the reviewed authority visible. The existing leaf parsing (last `.`-separated component) already handles branch names containing `/` and `.`, so no parsing change is required.

### 5.2 Preflight includes, then collect unsupported keys

**File:** `internal/gitx/service.go`  
**Functions:** `validateLocalConfiguration` and unexported repository-configuration path/scanning helpers

Refactor validation as follows:

1. Resolve `.git` directory or gitfile, an optional `commondir`, and the active `config` through bounded filesystem operations only, revalidating absent optional metadata.
2. When the resolved common `config` exists, read the stable bounded regular file, handle one leading UTF-8 BOM, collect actual `path` assignments in direct `include` and `includeIf` sections without touching their targets, and detect configuration that enables `extensions.worktreeConfig`. When `config` is absent, treat the repository-local configuration as empty only after revalidating the resolved metadata paths and the configuration's absence.
3. If include path assignments exist, normalize, deduplicate, and sort those keys; return the value-free rejection immediately, without starting the Git subprocess for that validation.
4. If `extensions.worktreeConfig` is enabled, return its value-free rejection without starting Git, preventing the unscanned `config.worktree` from being activated for static repository input. Disabled forms continue to the normal unsupported-key path.
5. Only when the filesystem preflight passes, run the existing protected `git config --local --no-includes --null --list` command.
6. Inspect every bounded record rather than returning on the first unsupported record, treating a record without a value separator as an empty value per section 4.2.
7. Add safely listable unsupported keys to a set, normalizing section and leaf to lowercase while preserving subsection casing.
8. Sort and return one value-free error, with the remediation line, when the set is non-empty.

Prefer small unexported helpers for bounded path resolution, scanning, and rejected-key formatting. Do not introduce a general validation framework or exported error type unless an existing caller requires structured inspection.

Structural safety errors remain distinct from policy rejection. Examples include malformed `.git` gitfiles or present `commondir` files, missing referenced Git-directory or common-directory targets, configuration replacement during inspection, oversized configuration, unreadable data, malformed Git output, or Git failure. An absent resolved common `config` is not a structural error when all Git metadata is otherwise valid and its absence is race-revalidated.

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
branch.feat/core-19.gh-merge-base=main
remote.origin.gh-resolved=base
```

Assert that `PrepareSource` succeeds and its bundle verifies.

Add negative boundaries proving that:

- similarly named unreviewed branch and remote keys remain rejected;
- unsafe scalar values are independently rejected for all four newly accepted shapes: `branch.*.vscode-merge-base`, `branch.*.github-pr-owner-number`, `branch.*.gh-merge-base`, and `remote.*.gh-resolved`;
- branch names containing `/` and `.` do not break key parsing;
- enabled `extensions.worktreeconfig` is rejected by filesystem preflight before Git, while a disabled form remains unsupported and is rejected through the normal aggregate path (section 4.5);
- a valueless implicit-boolean key resolves according to the empty-value rule in section 4.2;
- a rejected key under a mixed-case branch subsection is reported with its subsection casing preserved, and the reported key works verbatim in `git config --local --unset-all`.

### 6.2 Aggregated diagnostics

Create one repository-local configuration containing multiple safely listable rejected entries, including a duplicate but no include path assignment:

```text
core.fsmonitor
credential.helper
filter.payload.process
merge.payload.driver
credential.helper
```

Assert that:

- one error reports every distinct rejected key;
- keys are normalized and sorted per section 4.2;
- the duplicate appears once;
- no configured value appears in the error;
- the remediation line is present;
- none of the referenced executables run;
- no source artifact or host ref mutation occurs.

Update existing exact single-key assertions to the plural stable message. Keep the existing per-key security table so every dangerous key remains independently defended.

### 6.3 Include-first preflight and structural boundaries

Cover active direct include path assignments in each layout or encoding boundary:

- a normal `.git/config`;
- a `.git/config` whose first section header follows a leading UTF-8 BOM;
- a linked-worktree gitfile whose Git directory resolves through `commondir` to the active common `config`;
- a submodule-style gitfile whose Git directory directly owns `config`.

Combine direct include path assignments, duplicate include keys, and other unsupported keys in the same repository. Assert that the first error contains only the normalized, sorted, deduplicated include keys and no values; the Git runner records zero subprocesses; and a missing or deliberately unreadable include target does not change the policy error. Remove the include assignments and retry, then assert that the remaining safely listable unsupported keys appear in the aggregate error.

Add structural cases for the gitfile and `commondir` boundaries: malformed, missing, symlinked, and oversized `.git` gitfiles; malformed and oversized present `commondir` files; missing referenced Git-directory and common-directory targets; a symlink replacement of the resolved common `config`; and malformed include-section headers. Also cover an absent resolved common `config` as an empty local configuration when the metadata path is otherwise valid, including replacement during absence revalidation. Prove the repository-relative gitfile path and Git-directory-relative `commondir` path with a real linked-worktree layout. Assert that each structural/path/file failure remains distinct from policy rejection and that no Git subprocess starts. Keep active-include coverage for the submodule-style gitfile whose Git directory directly owns `config`.

### 6.4 Worktree configuration

Add explicit tests that configuration enabling `extensions.worktreeconfig` is rejected from the bounded common-config snapshot with zero Git subprocesses, so `$GIT_DIR/config.worktree` is not activated for the static fixture. Also prove that a disabled form does not activate the extra file but remains unsupported and is rejected by the normal aggregate allowlist path.

### 6.5 Command transport

Keep the `ext::` remote transport test and assert that the configured command leaves no execution sentinel. Update only its expected value-free error message if the aggregated formatter changes the single-key output.

## 7. Documentation plan

**File:** `docs/manual/user-guide.md`  
**Section:** `2.3 Repository and mount rules`

Add a concise subsection titled **Repository-local Git configuration** explaining:

- DSX runs host Git for restrictive source and result transfer;
- `.git/config` can change Git behavior and is therefore security-relevant;
- DSX accepts reviewed inert repository, identity, branch, and remote metadata;
- VS Code merge-base, GitHub PR-number, GitHub CLI merge-base, and GitHub CLI remote-resolution metadata are supported;
- command-bearing configuration, include path assignments, credential helpers, filters, drivers, hooks, and command transports remain blocked;
- the active common configuration is resolved without Git and direct include path assignments are reported first, without resolving or reading include targets; the user removes them and retries before receiving any aggregate safely listable rejection;
- configuration enabling `extensions.worktreeConfig` is rejected from that bounded snapshot before Git can activate `config.worktree`, while disabled forms remain unsupported through normal allowlist rejection;
- the snapshot/revalidation guarantee covers repository-controlled static input and observable mutation, not malicious concurrent mutation by the trusted local host user;
- safely listable unsupported keys are otherwise aggregated without exposing their values and with the `git config --local --unset-all` remediation;
- remediation is to remove an unsupported repository-local key or move an appropriate user preference to global configuration, not to disable DSX validation;
- a common concrete case: `git lfs install --local` writes command-bearing `filter.lfs.*` keys that DSX correctly rejects; run `git lfs install` globally instead.

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

Then verify against isolated temporary repositories containing all four supported metadata keys; unsafe values for each of those keys; multiple blocked command-bearing keys; actual include path assignments in normal, BOM-prefixed, linked-worktree, and submodule-gitfile layouts; enabled and disabled `extensions.worktreeConfig`; structural gitfile and `commondir` failures; and an `ext::` URL with an execution sentinel. Supported cases must create and verify a source artifact. Static include fixtures and an enabled worktree-configuration fixture must stop the relevant validation before Git and before include-target or `config.worktree` access. Other blocked cases must return sorted, deduplicated, value-free diagnostics as applicable and execute no configured command.

After focused verification, run the repository suite:

```console
go test ./...
```

This change affects workspace creation behavior. After implementation, build the host binary and run the required safe isolated-project Apple runtime creation-and-attach smoke test before claiming the change complete.

## 9. Acceptance criteria

1. `branch.*.vscode-merge-base` no longer blocks workspace source preparation when its value is a safe scalar.
2. `branch.*.github-pr-owner-number` no longer blocks workspace source preparation when its value is a safe scalar.
3. `branch.*.gh-merge-base` and `remote.*.gh-resolved` no longer block workspace source preparation when their values are safe scalars.
4. Unsafe values for each of those four exact shapes, plus arbitrary or similarly named branch and remote keys, remain rejected.
5. Actual direct `include.path` and `includeIf.*.path` assignments in normal, BOM-prefixed, linked-worktree, and submodule-gitfile layouts are reported by filesystem preflight without reading an include target or starting the Git subprocess for that validation.
6. Include keys are normalized, deduplicated, sorted, and value-free; they are reported alone in the first error. Removing them and retrying exposes any remaining safely listable unsupported keys.
7. Gitfile and `commondir` resolution is bounded, filesystem-only, and covered at malformed/path/file boundaries; an absent resolved common `config` is accepted as empty only after metadata and absence revalidation, while missing or malformed required pointers and targets remain structural errors distinct from policy rejection.
8. After the include preflight passes, all safely listable unsupported repository-local keys are reported in one deterministic error containing the `git config --local --unset-all` remediation.
9. Rejected keys are deduplicated and sorted, with section and leaf lowercased and subsection casing preserved, so a reported key works verbatim in `git config --local --unset-all`.
10. Rejected values never appear in errors or logs.
11. Configuration enabling `extensions.worktreeconfig` is proven rejected from the bounded common-config snapshot before Git, keeping `config.worktree` deactivated for repository-controlled static input; disabled forms are proven unsupported through the normal aggregate rejection.
12. Valueless implicit-boolean keys have deterministic, tested validation behavior.
13. Existing command-bearing configuration tests and the `ext::` transport test prove that configured executables do not run.
14. Source creation, bundle verification, result fetch, and guarded apply retain their existing protected Git environment and repository identity checks.
15. The manual explains include-first remediation, `extensions.worktreeConfig` handling, the trusted-host-user threat boundary, bounded gitfile/`commondir` resolution and BOM handling, the four exact supported metadata shapes, the reason command-bearing Git configuration remains blocked, and the git-lfs remediation.
16. An isolated Apple runtime workspace created from a repository carrying all four metadata shapes, attached successfully at `/workspace`, and was removed with exact owned-resource cleanup.
