# DSX user and operator guide

This guide describes the DSX multi-workspace command, configuration, security, and operator contracts. It does not by itself prove that an external release or physical-runtime evidence gate has passed. New users should begin with [Getting started with DSX](./getting-started.md).

## 1. Product model and requirements

DSX runs on Apple-silicon (`arm64`) Macs with macOS 26 or newer and uses Apple's `container` runtime. The initially supported compatibility range is `>=1.2.2 <1.3.0`; untested or mismatched CLI/API-server versions fail before mutation.

Every DSX workspace is:

- explicitly named;
- a peer of every other workspace;
- an Apple container/microVM;
- backed by guest-owned storage and independent Git metadata; and
- populated from committed local revisions through restrictive verified Git bundles.

The local Git checkout is the source and integration point, not a workspace. DSX never mounts host project source or the host home directory into a workspace. There is no implicit workspace, distinguished workspace, or workspace-mode choice.

Install the Darwin/ARM64 `dsx` and Linux/ARM64 `dsx-guest` binaries beside one another. The host verifies and stages the guest helper read-only. Check the installation without creating resources:

```console
$ dsx version
$ dsx doctor
$ dsx doctor --require-builder
```

DSX may direct an operator to Apple-native system or builder startup. It does not install, stop, uninstall, prune, or delete Apple's runtime, default network, or builder.

## 2. Project configuration and trust

### 2.1 Configuration location

Setup writes the normal home-local contract to:

```text
~/.dsx/projects/<project-name>-<project-id>/config.jsonc
```

A maintainer may instead explicitly use `.dsx/config.jsonc` as the repository-shared contract. Exactly one may be active. DSX fails on ambiguity and never merges configuration files.

Configuration precedence is:

```text
CLI override
  > one active project configuration
  > DSX standard default
```

`dsx inspect` is read-only. It shows detected facts, effective values, the source of each value, and the executable-configuration hash. A changed executable plan requires review. Non-interactive mutation must provide the exact `--approve-config` hash. `--force` never bypasses approval.

Dev Container declarations are not discovered, imported, parsed, or executed. Suggestions from lockfiles, Dockerfiles, Containerfiles, or `devenv.nix` remain inert until represented in reviewed DSX configuration.

### 2.2 Strict offline schema

`schema/dsx-config-v1.schema.json` is embedded and evaluated offline. Remote references are disabled, input is bounded, duplicate keys are rejected with source locations, and unknown fields fail rather than being ignored.

The workspace-model cutover is strict:

- `agents.allowed` and `agents.default` are the only project agent-selection authority;
- the default must be one of the allowed harnesses;
- portable host-import eligibility is declared with `auth.imports`;
- per-workspace volume scope is spelled `workspace`;
- concurrency is `resources.maxConcurrentWorkspaces`; and
- creation-time authentication, browser, profile, or workspace-mode defaults are not configuration fields.

A representative configuration is:

```jsonc
{
  "$schema": "https://dsx.dev/schema/config-v1.json",
  "schemaVersion": 1,
  "workspace": {
    "root": ".",
    "members": [
      { "name": "api", "path": "services/api" },
      { "name": "web", "path": "services/web" }
    ]
  },
  "image": { "standard": true },
  "setup": [
    { "argv": ["pnpm", "install", "--frozen-lockfile"], "cwd": "/workspace" }
  ],
  "processes": {
    "web": {
      "argv": ["pnpm", "dev"],
      "cwd": "/workspace/services/web",
      "required": true,
      "health": {
        "http": { "url": "http://127.0.0.1:3000/health" },
        "interval": "1s",
        "timeout": "2s",
        "retries": 30
      }
    }
  },
  "volumes": {
    "node-modules": {
      "target": "/workspace/node_modules",
      "scope": "workspace",
      "persistent": true
    }
  },
  "mounts": [
    {
      "source": { "type": "volume", "volume": "node-modules" },
      "target": "/workspace/node_modules"
    }
  ],
  "agents": {
    "default": "omp",
    "allowed": ["omp", "codex", "claude", "opencode"]
  },
  "auth": {
    "imports": ["omp", "codex", "opencode"]
  },
  "aws": {
    "mode": "host-default",
    "directory": "/Users/example/.aws"
  },
  "network": { "internet": true, "hostGrants": [] },
  "ports": [
    {
      "name": "web",
      "guest": 3000,
      "host": "dynamic",
      "bind": "127.0.0.1",
      "protocol": "tcp"
    }
  ],
  "resources": {
    "cpus": 6,
    "memory": "6GiB",
    "maxConcurrentWorkspaces": 4
  }
}
```

Structured `argv` is executed directly. It is not converted to a host shell string and does not depend on guest shell rc files. An explicitly declared shell command remains executable configuration and appears in approval review.

### 2.3 Repository and mount rules

`workspace.root` and `workspace.members` identify local Git repositories whose committed revisions are bundled into independent guest clones. A `mounts` source of type `workspace` resolves inside that private guest clone; it does not refer to a live host source mount.

Reviewed host-directory grants, where supported for narrow integrations, must be canonical and read-only. Semantic validation denies host home, project source, runtime sockets, SSH/GPG agents, Keychain, Tailscale state, browser profiles, temporary/runtime directories, symlink escapes, and the reserved `dsx-guest` target.

#### Repository-local Git configuration

DSX runs host Git to create restrictive source bundles and to fetch and apply workspace results. Repository-local Git configuration is an input to those operations, so it is security-relevant: Git configuration can run commands, load further configuration, or acquire objects over unexpected transports.

DSX therefore accepts only reviewed, inert repository-local keys: repository format facts, identity and preference scalars, and common branch and remote metadata. The four additional supported metadata shapes are exactly:

```text
branch.<branch>.vscode-merge-base
branch.<branch>.github-pr-owner-number
branch.<branch>.gh-merge-base
remote.<remote>.gh-resolved
```

These cover VS Code merge bases, GitHub pull-request numbers, and GitHub CLI merge-base and default-repository annotations. Their values must still be safe non-empty scalars; similarly named or command-oriented leaves are not accepted.

During a repository-configuration validation, DSX performs a filesystem preflight before starting the Git command associated with that validation. A physical `.git` directory selects its `config`. Otherwise, DSX accepts only a non-symlink regular gitfile of at most 64 KiB containing exactly one `gitdir: ` line with an optional LF or CRLF terminator; a relative target is repository-relative and must resolve to a canonical physical directory. An optional `commondir`, when present, has the same file, size, and one-line terminator constraints, resolves relative paths from the Git directory, and must also name a canonical physical directory. When `commondir` is absent, DSX race-revalidates that absence and uses the Git directory. DSX performs a bounded stable read of the resulting common `config` when it exists, stripping exactly one leading UTF-8 BOM there; gitfiles and `commondir` files do not accept a BOM. An absent resolved common `config` is a legitimate empty repository-local configuration only after DSX revalidates the Git directory, any present `commondir` pointer and target, and the configuration's continued absence. DSX inspects actual `path` assignments in direct `[include]` and `[includeIf "..."]` sections, never resolves, opens, or reads an include target, and does not treat an empty include section header alone as a policy violation. It does not rely on `--no-includes` as a listing-level defense, because Git may process repository configuration while setting up the repository command.

The same bounded common-config snapshot is checked for `extensions.worktreeConfig`, including its valueless implicit-true form. Direct include path assignments retain the first-error precedence described below. Once none remain, configuration that enables the extension is rejected during filesystem preflight, without starting the Git command for that validation. For repository-controlled static input, this prevents Git from activating the unscanned `$GIT_DIR/config.worktree`. Disabled forms do not activate that file, but the key remains unsupported and is rejected through the normal aggregate allowlist path after preflight.

The bounded snapshot and revalidation protect against repository-controlled static input and fail closed when a metadata or configuration mutation is observable during those checks. The local host user is trusted. DSX does not claim to defend against that user, or another process acting with that user's authority, concurrently replacing `.git` metadata after a check or while the following Git process starts.

If actual direct include path assignments exist, DSX reports their keys first, sorted and deduplicated without values, and does not start Git for that validation. Remove all include assignments and retry. Other unsupported keys are intentionally not merged into that first error; after include removal, DSX aggregates every safely listable unsupported key in one sorted, deduplicated, value-free rejection:

```console
$ dsx workspace create feature
dsx workspace create: repository-local Git configuration keys are not allowlisted: "include.path"; remove a key with: git config --local --unset-all <key>
$ git config --local --unset-all include.path
$ dsx workspace create feature
dsx workspace create: repository-local Git configuration keys are not allowlisted: "core.fsmonitor", "credential.helper", "filter.lfs.process"; remove a key with: git config --local --unset-all <key>
```

Command-bearing and object-acquiring configuration stays blocked: credential helpers, clean/smudge and process filters, external diff commands and merge drivers, filesystem monitors and hooks, and command remote transports such as `ext::`. A common concrete case is `git lfs install --local`, which writes command-bearing `filter.lfs.*` entries that can run Git LFS and alter object acquisition. Run `git lfs install` globally instead.

Remediate by removing an unsupported repository-local key or by moving a legitimate personal preference into your global Git configuration. Do not try to disable DSX validation. A missing required `.git` metadata entry or gitfile, a malformed `.git` gitfile or present `commondir` pointer, a missing referenced target, and symlinked, non-regular, replaced, unreadable, oversized, or non-canonical metadata paths and files are reported as structural errors rather than allowlist rejections. The only file-absence exception is the resolved common `config` described above; an absent optional `commondir` selects the Git directory, and both absences are race-revalidated.

## 3. Setup and TUI

### 3.1 Bare command and setup

With a terminal, bare `dsx` opens setup for an unconfigured project or the dashboard for a configured project. `dsx init [--root PATH]` opens the same setup flow directly. If stdin or stdout is not a TTY, bare `dsx` prints help and changes nothing; interactive setup fails rather than prompting invisibly.

Setup is a three-step flow:

1. Choose **Ubuntu — Default settings** or **Ubuntu — Custom**. Default uses Codex, 6 CPUs, 6 GiB, internet access, no published ports, and no browser. Custom exposes the default agent, internet policy, guest ports, CPU, and memory. Alternate images remain configurable outside this TUI. Setup also asks whether the project may allow selected workspaces to follow the host AWS `default`; this authorizes only the capability, never a workspace grant.
2. Review one concise approval screen. It shows the effective environment, resources, network policy, browser state, agent, ports, executable hash, and every non-default setup command, process, mount, credential import, host grant, or volume. For `host-default`, it also shows the approved canonical source and identity, reserved read-only guest destination, eligible profile `default`, new-workspace default **Disabled**, and dynamic identity warning. Routine internal digests, discovery lists, and provenance priorities are omitted. Long exceptional reviews scroll without truncation and cannot be approved before the complete content has been viewed.
3. Verify Apple Container, persist configuration and approval, prepare DSX Standard when required, and open the dashboard.

Authentication import remains a separate explicit approval. Setup does not silently import credentials. No configuration, approval, credential, or runtime resource is persisted before final confirmation. A post-confirmation Apple runtime preflight must succeed before project mutation.

Use `DSX_ACCESSIBLE=1` for accessible form mode. `NO_COLOR` is respected. Narrow terminals, resize, masked secret input, and terminal-safe rendering are part of the interface contract.

### 3.2 Dashboard

The dashboard shows:

- canonical local checkout branch, commit, and cleanliness;
- workspaces in deterministic order;
- lifecycle state and active mutation;
- source branch and revision;
- workspace default and project-allowed agents;
- final URLs and published ports;
- unfetched or unresolved-work warnings;
- AWS grant and non-secret availability state; and
- `Legacy — cleanup only` resources.

Actions for the selected workspace are state-aware:

| Key | Action |
|---|---|
| **c** | Create a workspace. |
| **Enter** | Open the selected workspace. |
| **a** | Open the agent form. |
| **u** | Update from the local checkout. |
| **s** | Start or stop. |
| **r** | Restart. |
| **g** | Review Git status or diff. |
| **d** | Remove. |
| **q** | Quit. |

The dashboard also exposes **Enable AWS** or **Disable AWS** for the selected workspace. These TUI actions record intent and use the same workspace lifecycle path as the CLI.

Update and restart are disabled while another lifecycle mutation is active. A workspace needing conflict resolution remains openable.

The create form contains only the validated name, recorded source branch/revision, and optional default agent selected from `agents.allowed`. It offers create-and-open or background creation. It never asks for credentials, a task prompt, browser selection, or a workspace mode.

After either create action, the TUI remains on a bounded milestone screen while DSX validates the approved plan and creates, starts, and bootstraps the workspace. **Create and open** hands directly from completed progress to the workspace shell without exposing an idle host prompt. **Create in background** completes after the same progress without attaching a shell.

The agent form contains only an agent selection and an **Enable isolated browser** checkbox. Browser selection is per session and is not stored as a workspace-creation default.

Before handing the terminal to an interactive workspace shell or agent, the TUI exits its alternate screen and restores normal terminal state. It may restore the dashboard after the child exits. Confirmed work shows bounded milestones rather than unbounded raw runtime logs.

## 4. Command reference

| Command | Contract |
|---|---|
| `dsx` | Open setup or the multi-workspace dashboard; print help without a TTY. |
| `dsx inspect` | Read-only effective plan, provenance, and configuration hash. |
| `dsx init` | Open the setup flow. |
| `dsx workspace create NAME` | Create a private-clone workspace from the committed local revision. |
| `dsx workspace create NAME --default-agent AGENT` | Set an approved workspace-specific default. |
| `dsx workspace list` | List current workspaces and legacy cleanup-only resources. |
| `dsx workspace open NAME` | Start if needed, wait for readiness, and open the shell. |
| `dsx workspace start NAME` | Start the workspace and only `dsx-guest`, without attaching. |
| `dsx workspace stop NAME` | Stop while preserving private state. |
| `dsx workspace restart NAME` | Stop and start without restoring agent or project processes. |
| `dsx workspace update NAME` | Rebase the workspace branch onto the latest committed local revision. |
| `dsx workspace remove NAME` | Remove one proven workspace subject to result protection. |
| `dsx workspace remove --all` | Remove removable current-project workspaces. |
| `dsx workspace remove --legacy-resources` | Remove proven current-project legacy resources. |
| `dsx workspace remove --all-projects` | Remove proven workspace resources across projects after confirmation. |
| `dsx agent WORKSPACE [--agent AGENT] [--browser] [-- PROMPT]` | Run an interactive or prompted agent session in an existing workspace. |
| `dsx auth status` | Report project authentication availability without secret values. |
| `dsx auth import --agent AGENT` | Review and import only the harness's allowed portable artifact. |
| `dsx auth login --agent AGENT` | Perform an explicit DSX-scoped login. |
| `dsx auth refresh --agent AGENT` | Refresh canonical project credentials through the harness adapter. |
| `dsx auth purge --agent AGENT` | Confirm removal of canonical credentials and inactive copies. |
| `dsx aws status WORKSPACE` | Report that workspace's grant and non-secret host-default availability or failure state. |
| `dsx aws enable WORKSPACE` | Grant one workspace access to the current and continuously refreshed host default. |
| `dsx aws disable WORKSPACE` | Revoke one workspace and remove its private AWS mirror. |
| `dsx git status WORKSPACE [--repo MEMBER]` | Show source, result, dirty, rebase, fingerprint, and fetch state. |
| `dsx git diff WORKSPACE [--repo MEMBER]` | Render bounded, terminal-safe changes. |
| `dsx git fetch WORKSPACE [--repo MEMBER]` | Import verified committed history into the named host ref. |
| `dsx git apply WORKSPACE [--repo MEMBER]` | Guard and apply a squashed working-tree result. |
| `dsx doctor [--require-builder]` | Read-only host/runtime compatibility checks. |
| `dsx version [--json]` | Print build and pinned component metadata. |

Every workspace operation requires a name unless it uses an explicit cleanup-set selector. The unreleased cutover has no compatibility aliases or unnamed lifecycle behavior.

Destructive operations prompt in a terminal. `--force` may explicitly confirm loss during removal, but it cannot bypass configuration approval, resource ownership, workspace identity, or bundle verification.

## 5. Workspace lifecycle

### 5.1 Names and states

Workspace names are 1–24 bytes of lowercase letters, digits, and hyphens, with no leading or trailing hyphen. The durable states are:

- `planned`;
- `creating`;
- `running`;
- `stopped`;
- `needs_resolution`;
- `failed`;
- `cleaning`; and
- `deleted`.

Mutations use per-workspace and project locks with a fixed ordering. Manifests are written before runtime mutation and use optimistic generations. Cancellation stops forward work and runs bounded rollback without losing the original error.

### 5.2 Create

```console
$ dsx workspace create feature-a
```

Creation:

1. verifies a clean tracked local checkout;
2. records its checked-out branch and commit;
3. warns that ignored and untracked files are excluded;
4. creates a restrictive source bundle in a private temporary location;
5. verifies the bundle and repository identities;
6. writes lifecycle intent before runtime changes;
7. creates isolated source, dependency, service, session, and network resources;
8. clones without object hardlinks or shared Git metadata;
9. checks out `dsx/feature-a`; and
10. starts only `dsx-guest`.

Finite approved setup commands may execute during creation, but no agent, browser, application, watcher, process manager, manually started database, or other persistent project process starts implicitly.

### 5.3 Open, start, and stop

```console
$ dsx workspace open feature-a
$ dsx workspace start feature-a
$ dsx workspace stop feature-a
```

`open` is permitted for `running`, `stopped`, and `needs_resolution` workspaces. It starts a stopped workspace, waits for guest readiness, and enters the shell. `start` performs the same state restoration without terminal attachment. `stop` terminates active agents and temporary helpers, removes live publications as appropriate, and preserves private clone data, credentials, dependencies, service volumes, configuration, and ownership.

The managed DSX Standard shell is login interactive Zsh with pinned, image-owned Antidote plugin content and pre-generated Starship initialization. Startup is offline. Node/npm/pnpm, Python/pip/venv, Go, and an LTS JDK are exported at image level so direct structured commands do not need rc files. Custom images remain responsible for their own shell and toolchain.

### 5.4 Restart

```console
$ dsx workspace restart feature-a
```

Restart transitions a running or stopped workspace through stopped to running. It preserves files, Git and rebase state, commits, uncommitted changes, dependencies, service volumes, credential copies, configuration, and ownership.

Restart terminates and never restores agent sessions, browsers, development servers, watchers, manually started databases, background commands, process managers, or application processes. Only `dsx-guest` starts automatically. Sibling workspaces remain untouched.

### 5.5 Update and conflict recovery

```console
$ dsx workspace update feature-a
```

Update means **Update from local checkout**. It requires:

- a clean, committed local checkout;
- the same checked-out source branch recorded for the workspace;
- a verifiable restrictive source bundle; and
- a workspace branch that can be safely rebased.

DSX transfers the latest source revision, verifies it, records a backup ref, and rebases `dsx/feature-a`. It does not stash uncommitted files, synthesize commits, merge unrelated branches, or attempt semantic resolution.

On conflict the manifest durably records the conflict, the state becomes `needs_resolution`, and the valid Git rebase state is preserved:

```console
$ dsx workspace open feature-a
$ git status
$ git rebase --continue
# or
$ git rebase --abort
```

Opening remains allowed for recovery. Re-run Git status afterward. An update does not affect any sibling workspace.

### 5.6 List and remove

```console
$ dsx workspace list
$ dsx workspace remove feature-a
```

List output includes source revision, lifecycle state, default and allowed agents, URLs, mutation state, warnings, result/fetch state, and legacy cleanup-only records. Ordering is deterministic and rendered text is terminal-sanitized.

Removal inventories the exact manifest and inspected runtime resources, verifies ownership labels, respects reverse dependencies, and deletes only proven resources. It is idempotent after interruption or partial startup.

Removal refuses uncertain or unexported work, including:

- unfetched commits;
- uncommitted files;
- in-progress or conflicted rebase state;
- a result bundle not confirmed imported; or
- a guest whose result state cannot be established.

Fetch or apply first. An explicit loss confirmation may discard work, but no option bypasses ownership proof. Canonical project credentials survive workspace removal.

## 6. Agent lifecycle

An agent always targets an existing workspace:

```console
$ dsx agent feature-a
$ dsx agent feature-a --agent codex
$ dsx agent feature-a --agent omp -- "implement the API"
```

Resolution order is explicit override, workspace default, then project default. Every choice must occur in `agents.allowed`. An override applies only to that invocation.

No prompt opens an interactive session. Text after `--` is passed as the task prompt using the harness's exact structured argument contract. DSX streams output, propagates exit status, signals, cancellation, and terminal resize, and preserves the workspace when the agent exits. Repeated sessions reuse the same files and processes. An agent request never creates a workspace.

Workspace stop or restart terminates active sessions and never restores them. Multiple workspaces may run different harnesses concurrently without writable Git, auth, session, or network sharing.

## 7. Authentication lifecycle

### 7.1 Import allowlists

`auth.imports` may contain only `omp`, `codex`, and `opencode`. It is onboarding policy, not an instruction to copy files. Each command presents the discovered source and exact artifact set for separate approval:

```console
$ dsx auth status
$ dsx auth import --agent omp
$ dsx auth import --agent codex
$ dsx auth import --agent opencode
```

The artifact allowlist is exact:

| Harness | Import contract |
|---|---|
| OMP | A consistent snapshot taken with `agent.db` closed, plus the optional WAL belonging to that snapshot. |
| Codex CLI | Only its approved portable `auth.json`. |
| OpenCode | Only its approved provider-auth artifact. |
| Claude Code | Host import unsupported; perform DSX login. |

DSX does not copy complete harness directories, adjacent unapproved files, host home contents, macOS Keychain data, or Claude host state. It never imports silently. OMP's embedded Codex identity remains OMP data and is never converted to Codex CLI format.

Import uses restrictive temporary files and a canonical per-project store separated by harness. Secret values never enter configuration, plan hashes, manifests, logs, errors, TUI output, or process arguments.

### 7.2 Login, refresh, copies, and purge

```console
$ dsx auth login --agent claude
$ dsx auth refresh --agent omp
$ dsx auth purge --agent omp
```

Login is explicit and DSX-scoped. A temporary callback bridge, when supported, belongs only to that login and is removed on completion or cancellation.

The first agent session for a harness lazily seeds an independent writable workspace credential copy from the canonical project store. Only that harness's artifacts are injected. Writable copies are never mounted concurrently into multiple workspaces. Promotion back to the canonical store is serialized; a conflicting refresh is preserved or rejected rather than generically merging secrets.

Purge is a separate confirmed operation. Active copies block it until the relevant sessions stop or the safe shutdown path is followed. Ordinary workspace removal does not purge canonical credentials.

## 8. Browser-session lifecycle

Browser support is an invocation-level choice:

```console
$ dsx agent feature-a --browser
$ dsx agent feature-a --agent codex --browser -- "exercise the application"
```

For each enabled session, DSX:

1. creates one new disposable browser VM;
2. attaches it only to the selected workspace's private network;
3. waits for the pinned Playwright MCP endpoint;
4. injects one ephemeral MCP configuration into that session;
5. publishes no browser control port to the host; and
6. removes the browser on success, error, cancellation, or terminal closure.

The browser has no source, harness credentials, AWS state, host home, runtime socket, host-control publication, or mounts copied from the workspace. It is never created during workspace creation, shared between workspaces, reused between sessions, persisted, or restored after workspace restart.

## 9. Git update and result integration

All Git operations name a workspace:

```console
$ dsx git status feature-a
$ dsx git diff feature-a
$ dsx git fetch feature-a
$ dsx git apply feature-a
```

`status` reports recorded source branch/revision, `dsx/feature-a`, dirty state, rebase/conflict state, host fingerprint, last fetched revision, and unexported work.

`diff` safely renders committed and uncommitted changes, omits unsafe terminal control sequences, describes binary changes without dumping their content, and bounds output.

`fetch` creates and verifies a restrictive result bundle and imports committed history to:

```text
refs/remotes/dsx/feature-a
```

It does not merge. The user may merge the remote-tracking ref normally. `apply` is a convenience that checks the recorded host fingerprint and ref state before applying a squashed working-tree result. It refuses without partial host mutation on mismatch. New, deleted, renamed, and binary files are preserved.

Composite projects accept `--repo MEMBER`. Cross-repository atomicity is not claimed. DSX never semantically merges parallel results.

A recommended parallel flow is:

1. Create `feature-a` and `feature-b`.
2. Run independent agents.
3. Commit new local work.
4. Update both workspaces.
5. Fetch and merge `feature-a`.
6. Update `feature-b` from the newly merged local branch.
7. Fetch and merge `feature-b`.

## 10. Processes, networking, AWS, and ports

### 10.1 Guest processes

The integrated workspace may run agents, application processes, workers, MySQL, Redis, Caddy, or a configured project process manager. Processes share guest loopback and the workspace trust boundary. Configured dependencies and health checks gate explicitly requested actions. Output retained by DSX is bounded and process-labeled.

Workspace create, start, and restart do not implicitly restore long-running project processes. DSX performs no automatic project-process restart in the MVP. A process manager explicitly run by the user may apply its own policy.

### 10.2 Internet and private destinations

`network.internet` is explicit project policy. Every `network.hostGrants` entry names one hostname-or-IP destination and TCP port. The workspace receives only a scoped relay endpoint, never host identity, Tailscale state, a generic proxy, or runtime control. Relays are tied to the owning workspace lease and stop with it.

Sibling workspace networks are not bridged. A browser connects only to the selected workspace network.

### 10.3 Default-only host AWS

The project configuration has only two AWS modes:

- `aws.mode: "none"` (the default) authorizes no host AWS access; and
- `aws.mode: "host-default"` authorizes the project capability to offer selected workspaces the standard host AWS `default` from the approved canonical `aws.directory`.

No profile name is configurable in this increment. `host-default` extracts only `[default]` from the standard credentials and config files, does not set `AWS_PROFILE`, and ignores all named sections. Named profiles, `--profile` switching, and identity pinning are future work.

Project setup does not enable AWS in any workspace. Every new workspace starts with AWS access disabled. The approval must say that the capability is for selected workspaces only; new workspaces remain disabled; Leapp Desktop (or a compatible provider) must keep one complete temporary `default` active for enablement and rotation; switching the host default changes every AWS-enabled running workspace without another DSX approval or workspace restart; and named profiles are unavailable. The approved canonical source directory and identity, reserved guest destination `/run/dsx/aws`, read-only mode, eligible profile `default`, default-off state, and `dynamic-host-default` authority model are executable authority covered by the project approval hash. Host availability and credential bytes are not.

DSX integrates only with the provider's standard-file output. It never starts, stops, logs into, or otherwise controls Leapp or another provider. Enabling requires a valid complete temporary host `default`:

```console
$ dsx aws status feature-a
$ dsx aws enable feature-a
$ dsx aws status feature-a
$ dsx aws disable feature-a
```

`status` exposes the durable workspace grant plus only `available`, `unavailable`, or a stable non-secret failure code. It never emits credential values. The dashboard's selected-workspace **Enable AWS** and **Disable AWS** actions are equivalent to the CLI routes; the TUI records intent and uses the same lifecycle path.

An enabled running workspace owns a private mirror helper. It takes bounded stable snapshots, filters `default`, and atomically publishes complete config-and-credentials generations read-only at `/run/dsx/aws`. A stable replacement of the host default propagates continuously to every AWS-enabled running workspace without a DSX command, reapproval, or restart. This is deliberately dynamic authority: the effective account or role may change when the provider switches `default`.

The workspace grant persists across stop and restart. Stop terminates the helper and live publication while preserving the non-secret grant. Start and restart perform a fresh complete sync before exposing a shell or agent. Disable records revocation before terminating the helper and deleting that workspace's exact mirror. Remove cleans up the exact grant, helper, and mirror along with the proven workspace resources. A stable removal of host `default` revokes published credentials rather than retaining stale bytes.

An AWS-disabled workspace has no AWS files, AWS environment, mirror helper, or host-source access. Siblings do not share mirror state, and enabling or disabling one leaves the others' grants unchanged. Browser VMs never receive AWS files, environment, or mirror access, even for an agent session attached to an AWS-enabled workspace.

If enable, start, or restart reports that the host default is unavailable, start or renew one temporary `default` session in Leapp Desktop (or a compatible provider), then run `dsx aws status WORKSPACE`. For a source-identity failure, restore the reviewed canonical directory; changing it requires project reapproval. For an unexpected AWS identity, inspect the provider's active `default` and disable affected workspaces—the first increment intentionally follows that dynamic alias.

### 10.4 Published ports

Each `ports` entry names a guest TCP port and chooses `"dynamic"` or a fixed host port. Omitted bind defaults to `127.0.0.1`; a non-loopback bind is an explicit reviewed trust grant. Runtime bind results are authoritative.

Dynamic loopback ports are recommended for parallel workspaces. Fixed ports must not collide. Final mappings and URLs are reported per workspace. Port reconfiguration may replace only the selected runtime container after confirmation while retaining private clone, network, volumes, credentials, and ownership. That replacement terminates agent and project processes and does not restore them automatically.

## 11. Naming, ownership, cleanup, and legacy state

### 11.1 New resource names

New runtime container names follow:

```text
dsx-<project:16>-<workspace:24>-<role:9>-<path-hash:6>
```

Examples:

```text
dsx-tracking-chrome-feature-a-workspace-a81f2c
dsx-tracking-chrome-feature-b-workspace-a81f2c
dsx-tracking-chrome-feature-a-browser-a81f2c
```

The project folder component is sanitized and limited to 16 characters; workspace to 24; role to 9; and deterministic canonical-path hash to 6. The complete name is at most 62 bytes. Sanitized display collisions remain distinct through canonical identities and ownership metadata.

Names improve readability but never authorize mutation. DSX requires authoritative ownership labels plus a matching write-ahead manifest containing project, workspace, and run identity. Ambiguous resources are reported and preserved.

### 11.2 Legacy cleanup only

Previous-model manifests and owned resources may be recognized only for safe inspection and removal. They retain their existing names and are shown as `Legacy — cleanup only`.

They cannot be started, opened, restarted, updated, adopted, migrated, renamed, or targeted by an agent. Cleanup requires the same ownership proof and result protection as a current workspace:

```console
$ dsx workspace remove --legacy-resources
```

A legacy record never causes an unrelated resource to be classified as DSX-owned.

### 11.3 Cleanup sets and unfetched guard

```console
$ dsx workspace remove feature-a
$ dsx workspace remove --all
$ dsx workspace remove --all-projects
```

Cleanup removes only the proven selected scope: VM/container, private network, ports, proxies, private clones, workspace caches, dependency and service volumes, logs, temporary files, helper processes, and lifecycle manifest. It is safe after cancellation, terminal closure, partial creation, partial cleanup, and stale runtime state.

The unfetched-work guard applies to current and legacy resources. Cleanup fails closed when result state is dirty, conflicted, unfetched, unexported, or uncertain. An explicit loss confirmation may discard work; it never weakens ownership checks. Apple builders, default networks, unrelated resources, another project's state, and ambiguous records are never deleted.

Canonical project credentials remain until a separate `dsx auth purge`.

## 12. Security boundary

Treat workspace code, agents, dependencies, setup commands, hooks, skills, plugins, MCP servers, shell commands, process declarations, and browser content as untrusted.

Required controls include:

- non-root workspace execution by default;
- no host source or host-home mount;
- no host dotfile read, copy, mount, import, or execution;
- no Apple runtime socket, SSH/GPG agent, Keychain, or Tailscale state;
- independent Git objects and writable state for each workspace;
- distinct source, authentication, reusable configuration, session, dependency, and service-data stores;
- structured subprocess argv rather than constructed host shell commands;
- restrictive temporary files for Git and authentication transfer;
- exact per-harness credential allowlists and separate approval;
- browser isolation from source, credentials, AWS, and host control;
- loopback publication by default;
- bounded I/O and terminal-safe rendering;
- write-ahead manifests, optimistic generation checks, and deterministic lock ordering;
- cancellation rollback on a bounded independent context; and
- ownership proof before every mutation or deletion.

An agent can read and change anything deliberately placed in its private workspace and can exfiltrate reachable data when internet access is allowed. Processes inside one workspace share a trust boundary. DSX does not provide task scheduling, semantic merge coordination, destination egress filtering, Docker Engine APIs, Docker Compose, nested containers, Kubernetes, Rosetta, or amd64 emulation.

## 13. Physical runners and release evidence

Destructive Apple acceptance runs belong only on dedicated physical Apple-silicon runners. A run must attest the host/runtime, acquire the host-local lock, write a unique ledger before mutation, inventory unrelated sentinels and builder identity, clean only exact proven IDs, and emit evidence.

Any uncertain ledger, ownership tuple, runtime state, sentinel, builder identity, or upstream run status quarantines the host. Only a human operator and independent reviewer may clear the exact stale marker or lock after reviewing evidence. Broad pruning, runtime shutdown, uninstall, default-network deletion, and builder deletion are never recovery actions. The [runner operations guide](../runner-operations.md) is authoritative.

Implemented code and release support are distinct claims. Registry publication identity, digest-pinned production images, Apple signing and notarization identity, provisioned macOS 26/27 runners, real provider authentication, PTY behavior, browser isolation, network relay, Leapp rotation, fault cleanup, and end-to-end workflow evidence must all pass their applicable gates before release support is claimed. Hosted macOS CI may compile and run non-virtualized checks but is not evidence for nested Apple virtualization.
