# ADR 0001: DSX implementation architecture

- **Status:** Proposed
- **Date:** 2026-08-09
- **Decision type:** Reversible early; increasingly costly after public configuration and release compatibility commitments
- **Owner:** DSX maintainers
- **Confidence:** 80%
- **Related:** [DSX Product Requirements](../PRD.md)

## Context

DSX must provide a fast, minimal, Apple-native command-line workflow for starting isolated Linux development workspaces and coding-agent harnesses from existing macOS project directories.

The implementation must coordinate:

- Apple's `container` runtime.
- OCI image build and reuse.
- Interactive terminals and signal propagation.
- Multiple named, isolated agent sandboxes for one project.
- Independent Git source transfer, branches, and result retrieval.
- Multiple guest processes, including agents, applications, MySQL, Redis, and Caddy.
- Project configuration and safe import of existing declarations.
- Persistent authentication and project state without concurrent writable sharing.
- Browser and temporary host-network integration.
- Deterministic ownership and cleanup after normal completion or failure.

Apple's `container` CLI is written in Swift and uses Apple's Containerization Swift package. That does not require clients of the installed CLI to use Swift. Apple describes the project as actively evolving and limits stability guarantees across some releases, making direct package coupling a material maintenance risk. See [Apple container](https://github.com/apple/container/blob/1.2.2/README.md).

## Decision drivers

1. Fast startup and low idle overhead.
2. A single installable host executable.
3. Reliable process, terminal, signal, and network handling.
4. Ability to build a small Linux ARM64 guest helper from the same repository.
5. A narrow, version-checkable boundary with Apple's runtime.
6. Minimal permanent DSX host state and no mandatory DSX daemon.
7. Parallel agent isolation without adding a scheduler or orchestration service.
8. Maintainability by a small team.
9. A path to additional runtimes without changing product semantics.
10. A discoverable first-run experience without weakening the explicit, scriptable CLI.

## Decision

### 1. Implement DSX in Go

Build two executables from one Go module:

- `dsx`: the macOS ARM64 host CLI and temporary host helper entry points.
- `dsx-guest`: a static Linux ARM64 guest lifecycle helper distributed with `dsx` and mounted read-only into every workspace.

Go is selected because it provides self-contained binaries, fast startup, mature process and networking primitives, straightforward cross-compilation, and one language for both macOS control-plane and Linux guest components. The guest build will use `CGO_ENABLED=0` so it does not depend on the selected project image's libc.

Swift will not be used for the main CLI. A narrowly scoped Swift helper may be introduced later only if a required macOS API cannot be used securely through a stable command or system interface.

### 2. Treat the Apple `container` CLI as the runtime boundary

DSX will invoke `container` commands as structured subprocess argument arrays. It will not:

- Import the Containerization Swift package.
- Copy or fork Apple's CLI implementation.
- Expose Apple runtime control into the guest.
- Scrape human-formatted output when machine-readable output is available.

The runtime adapter will:

- Require macOS 26 or newer and initially accept Apple `container` versions `>=1.2.2 <1.3.0`.
- Reject untested runtime versions until the self-hosted compatibility suite passes.
- Verify required Apple system services and capabilities without claiming those services are DSX daemons.
- Use machine-readable inspection results where available.
- Treat command exit status, bind results, and runtime inspection as authoritative.
- Keep Apple-specific arguments inside one package.

Proposed package boundary:

```text
internal/runtime/runtime.go
internal/runtime/apple/adapter.go
```

The internal runtime interface will represent DSX operations rather than mirror Docker or Apple command syntax:

```text
BuildImage
CreateWorkspace
StartWorkspace
Exec
Inspect
Stop
Delete
CreateVolume
DeleteVolume
CreateNetwork
DeleteNetwork
```

This preserves the option to add a Podman or Docker compatibility backend later without making it an MVP dependency.

### 3. Use a daemonless DSX host control plane

The `dsx` process will plan and perform operations directly. DSX will not install a permanent background daemon for the MVP. Apple's own launchd-managed `container` services remain prerequisites and are verified by `dsx doctor`.

Temporary helper modes may be spawned by the same binary for:

- OAuth callback forwarding.
- macOS browser opening.
- Explicit host-network proxying.
- Active-run cleanup coordination.

Each helper must be tied to a project, sandbox, and run lease and exit when the owning sandbox stops. Persistent state will be limited to configuration approval records, resource manifests, and explicitly persistent authentication/cache data.

### 4. Run one integrated workspace container per sandbox

The default runtime unit is one Apple container/VM for each live workspace or named clone sandbox. It contains:

- The selected agent harness.
- Other installed harness CLIs.
- A live source mount or guest-owned private clones.
- Application processes.
- Sandbox-specific infrastructure such as MySQL and Redis.
- A guest lifecycle helper or an explicitly selected project process manager.

Container-local processes communicate over loopback. User-facing ports are selectively published to macOS; clone sandboxes use dynamically allocated host ports unless fixed ports are explicitly required.

This avoids one Apple VM per ordinary local service, reducing startup time, memory, network configuration, and cleanup complexity. It accepts that processes inside one sandbox do not have isolation from one another.

One project may have one live-mounted workspace and multiple named clone sandboxes, but the live workspace and managed clone runs may not start concurrently in the MVP. Multiple named clone sandboxes may run concurrently because each has independent source, writable state, services, ports, and resource limits. DSX provides isolation and lifecycle only; it does not schedule tasks, coordinate prompts, or merge results.

Each sandbox that requests automated browsing receives a separate disposable browser Apple container/VM. It exposes a DSX-managed Playwright MCP endpoint on that sandbox's private network and receives no source, provider authentication, or AWS mounts. DSX injects only ephemeral per-run MCP configuration into the selected harness.

### 5. Use a mounted guest lifecycle helper with an exec control channel

`dsx-guest` will be bind-mounted read-only at `/usr/local/libexec/dsx/dsx-guest` in every standard or custom project image and will provide the stable primary guest process. Runtime injection is authoritative even if the standard image contains a copy.

It will:

- Start configured commands without a guest shell unless the command explicitly requests one.
- Track process identity and health.
- Multiplex child stdout/stderr to container output with process-name prefixes.
- Propagate termination signals and reap child processes.
- Report readiness and exit status.
- Mark the sandbox failed when a required process exits; DSX performs no automatic restart in the MVP.
- Remain independent of any one agent harness.

`dsx-guest` will own a guest-local Unix socket at `/run/dsx/control.sock`. The host invokes `container exec … dsx-guest ctl <operation> --json`; that short-lived guest command talks to the socket. Status, health polling, and cancellation therefore require no published control port, vsock dependency, or host control socket inside the guest. Runtime inspection detects whole-container failure.

When a project already uses a process manager such as `process-compose`, DSX may launch it without its TUI as a configured child rather than duplicate its process graph. Its output must retain process identity or be labeled as one process-manager stream.

Agents will normally be launched through `container exec` after required services become healthy.

### 6. Use explicit JSONC configuration with home-local ownership by default

Setup writes the project contract to `~/.dsx/projects/<project-name>-<project-id>/config.jsonc`. The readable project name is paired with the canonical-root-derived project ID to prevent collisions. A repository `.dsx/config.jsonc` remains an explicit, shareable team contract. DSX accepts exactly one location and fails closed when both exist; it does not merge them.

Configuration will describe:

- Workspace root and member repositories.
- Image/build inputs.
- Setup commands.
- Processes and services.
- Health checks and dependencies.
- Mounts and persistent volumes.
- Agent defaults and authentication profiles.
- Browser mode.
- Network trust grants.
- Published ports.
- CPU, memory, and clone-sandbox concurrency limits.

Configuration precedence is:

```text
CLI flags
  > one active DSX configuration (home-local or shared)
  > explicitly imported supported configuration
  > DSX defaults
```

DSX may suggest configuration from lockfiles, Dockerfiles, Dev Container files, or `devenv.nix`, but it will not silently execute inferred commands. Unsupported or dangerous imported fields must fail visibly or require explicit approval. Interactive users approve the executable configuration hash; unattended runs must provide the exact hash through `--approve-config`. `--force` never bypasses configuration trust.

### 7. Use cached OCI image layers

The image model is:

```text
Pinned Ubuntu base
  → common DSX development layer
  → project toolchain/system dependency layer
  → selected project image
```

Supported agent harnesses are included in the standard agent-ready image for fast switching. A release binary resolves that image through its published immutable digest. When release metadata is absent, a development binary embeds the DSX-owned Containerfile and harness lock, includes their complete build-context digest in the approved execution plan, materializes them in a private directory outside the project, builds a content-addressed `dsx.local/standard:<input-digest-prefix>` image after final confirmation, and reuses that tag on subsequent starts. Harness attestation accepts this path only when the plan selects the managed standard image and its input digest matches the embedded authority. Project and custom builds remain outside that trust path.

`dsx-guest` is still mounted at runtime so custom images and host/guest versions stay aligned. Project setup state is reusable only when its declared inputs and configuration hash have not changed.

### 8. Support live mounts and multiple named private clones

- `dsx shell` defaults to one live read-write mount for interactive work.
- `dsx run --name <name>` creates or resumes a named private clone for autonomous work.
- Multiple named clone sandboxes may run concurrently for one project.
- Host Git worktrees are not the secure boundary because they share the host repository's object database and metadata.

This follows Docker Sandboxes' separation between direct and clone modes while avoiding a persistent read-only host source mount: [Docker Sandbox architecture](https://docs.docker.com/ai/sandboxes/architecture/), [Git workspace modes](https://docs.docker.com/ai/sandboxes/usage/#git-workspace-modes).

Clone mode requires a clean tracked host working tree for the MVP. DSX creates a restrictive temporary Git bundle for the selected ref, copies it into the sandbox, and clones into a guest-owned volume without a persistent host source mount or shared object hardlinks. Ignored and untracked files are excluded and reported. Composite workspaces repeat this operation for each explicitly declared member repository while preserving relative paths.

Each sandbox uses an independent branch such as `dsx/<sandbox-name>`. Before result retrieval, DSX creates a final generated commit for any remaining tracked or untracked agent changes. Binary files, renames, additions, and deletions are therefore represented in the branch.

All user-facing Git operations live under one namespace:

```text
dsx git status [name]
dsx git diff <name> [--repo <member>]
dsx git fetch <name> [--repo <member>]
dsx git apply <name> [--repo <member>]
```

`dsx git fetch` exports a Git bundle from the sandbox and imports it into `refs/remotes/dsx/<sandbox-name>` in the host repository. This preserves agent commits plus the generated final commit without creating a persistent Git daemon or host remote. `dsx git apply` is a convenience that applies the result as a squash only when the host tracked-state fingerprint still matches sandbox creation; otherwise it refuses without modifying the host. Composite workspace Git operations require an explicit member or separate per-member execution and do not claim cross-repository atomicity.

Cleanup refuses to destroy a clone sandbox with unfetched results unless the user explicitly confirms loss or passes `--force`.

### 9. Separate authentication, configuration, sessions, and caches

Use distinct volumes or mounts for:

- Provider authentication.
- Reviewed reusable skills/configuration.
- Session history.
- Dependency caches.
- Sandbox service state.

Reviewed reusable configuration may be shared read-only. Each concurrent sandbox receives its own writable authentication/session volume seeded from the selected profile; one writable profile volume is never concurrently mounted into several sandboxes.

Global authentication survives `dsx clean` by default. Explicit `dsx clean --purge-auth` is required to remove selected authentication.

### 10. Track ownership using runtime metadata and local manifests

Each resource name and supported label will include a DSX namespace, project ID, sandbox name, run ID, and resource type. A local atomic manifest records the intended resource graph, Git fetch state, and cleanup state.

Runtime inspection and labels are the source of truth when a manifest is stale. DSX will not require SQLite for the MVP; atomic per-run JSON manifests and file locking are sufficient for the initial lifecycle.

Cleanup behavior:

- `dsx stop --name <name>` stops one sandbox while preserving configured persistent state.
- `dsx clean --name <name>` removes one sandbox after protecting unfetched results.
- `dsx clean` removes all current-project sandboxes and live-workspace resources DSX owns.
- `dsx clean --all` removes resources across DSX projects after confirmation.
- Apple runtime-owned builder resources are never classified as project resources or deleted by DSX.
- Ambiguous ownership is reported and left intact.
- Repeated cleanup is safe.

### 11. Treat performance and builder state as explicit runtime concerns

Apple's shared BuildKit builder is runtime-owned global infrastructure. `dsx doctor` verifies it when a build is required; cleanup never deletes it. Timing reports builder startup separately.

Initial p95 budgets on a warm host are: `dsx inspect` within 500 ms, DSX planning within 250 ms, a cached empty `dsx shell` prompt within 3 seconds excluding setup/services, ordinary cleanup within 5 seconds, and no more than 100 MiB combined for DSX host/guest helpers. These are acceptance budgets to validate, not claims about unmeasured Apple VM or project process usage.

### 12. Use a context-aware terminal UI for bare `dsx`

When stdin and stdout are interactive terminals, bare `dsx` will launch a terminal UI in the same executable:

- No project configuration and no DSX resources: setup wizard.
- A configured project: one state-driven project screen reports whether Apple Container is installed, stopped, running, or unavailable and whether the live workspace is absent, stopped, running, or unverifiable.

The project screen derives one primary action from those states. It never renders inapplicable lifecycle or Git commands. Advanced clone, stop, cleanup, and named-clone Git operations live under a secondary **More options** view. Starting the Apple container system is an explicit user action. `dsx init` opens the setup flow directly. Without an interactive terminal, bare `dsx` prints help and exits without prompting or changing state.

The TUI will use [Bubble Tea](https://github.com/charmbracelet/bubbletea) as its state/update/view framework and [Huh](https://github.com/charmbracelet/huh) for setup forms, with Bubbles and Lip Gloss components only where needed. Huh's accessible mode supplies the initial screen-reader path.

The TUI is a presentation adapter, not a second control plane:

```text
Explicit CLI commands ─┐
                       ├→ application services → planner → runtime/Git adapters
TUI actions ───────────┘
```

The setup flow performs detection and planning without mutation, then shows selectable DSX-standard and detected project image sources, the effective configuration, executable commands, mounts, credentials, network grants, ports, and configuration hash. Final confirmation first triggers a read-only Apple container-system status check. A missing CLI or service state other than `running` fails before configuration or approval persistence and directs the user to install the supported runtime or run `container system start`. Only after that preflight may setup write the home-local project configuration, persist approval, build the managed standard image, or invoke other resource creation. The standard-image build uses only embedded DSX-owned inputs and never includes project files. A repository `.dsx/config.jsonc` is an explicit shared alternative. Configuration approval and destructive cleanup rules are identical in CLI and TUI paths.

The project screen supports project/workspace status, create, attach, start, stop, clean, and the existing `dsx git status`, `dsx git diff`, and `dsx git fetch` operations through contextual actions. It does not embed logs, agent chat, task scheduling, or a full configuration editor.

Before handing the terminal to `container exec -it`, the TUI exits its alternate screen and restores normal terminal state; it may restore the dashboard after the child exits. Ctrl-C before confirmation is side-effect free, while interruption during creation invokes the same rollback path as explicit commands.

## Architecture

```text
┌──────────────────────────────── macOS ────────────────────────────────┐
│ User → CLI commands or Bubble Tea TUI                                 │
│                 ↓                                                    │
│         application services → Config/Planner                        │
│                                      ↓                               │
│                            Apple adapter → container CLI              │
│                                                                      │
│ Source repo → temporary Git bundles                                  │
│ Results    ← fetched Git bundles                                     │
│ State: approval hashes + project/sandbox/run manifests               │
│ Temporary helpers: OAuth/browser/network proxy                       │
└───────────────────────────────┬──────────────────────────────────────┘
                                │
                  ┌─────────────┴─────────────┐
                  ▼                           ▼
┌──────── named clone sandbox: api ───────┐  ┌──── named clone sandbox: tests ────┐
│ dsx-guest + Codex                       │  │ dsx-guest + Claude                  │
│ guest-owned clone: dsx/api              │  │ guest-owned clone: dsx/tests        │
│ app + sandbox-specific MySQL/Redis      │  │ app + sandbox-specific MySQL/Redis │
└──────────────────┬──────────────────────┘  └──────────────────┬──────────────────┘
                   │ private network                            │ private network
                   ▼                                            ▼
          disposable browser VM                       disposable browser VM
          Playwright MCP; no source/auth               Playwright MCP; no source/auth
```

## Options considered

### Implementation language

| Option | Benefits | Costs | Decision |
|---|---|---|---|
| Go | Fast, self-contained host binary; mature CLI/process/networking; easy Linux ARM64 guest build | Less direct access to Apple-only frameworks; configuration types less expressive than TypeScript | **Selected** |
| Swift | Native Apple APIs; same ecosystem as `container`; self-contained macOS binary | Linux guest build and shared code are harder; direct Containerization coupling is unstable; Apple-only implementation | Not selected for the main CLI |
| TypeScript with Bun | Fastest product iteration; strong JSON/config and agent ecosystem; standalone Darwin/Linux executables are supported | Larger embedded runtime; less conservative choice for PID 1, PTY, proxy, and signal-critical code | Viable fallback, not selected |
| Rust | Strong performance and safety; self-contained binaries | Higher implementation complexity and slower iteration for this product | Not selected |

Bun's standalone executable capability remains a valid alternative if Go materially slows product iteration: [Bun single-file executable documentation](https://bun.sh/docs/bundler/executables).

### Runtime integration

| Option | Benefits | Costs | Decision |
|---|---|---|---|
| Invoke Apple `container` CLI | Small surface; uses installed signed runtime; tested version range can be enforced; language-independent | Subprocess overhead; requires self-hosted compatibility tests across supported releases | **Selected** |
| Import Containerization Swift package | Typed low-level integration; native access | Tight coupling to evolving Apple APIs; requires Swift control plane; duplicates CLI policy | Not selected |
| Operate Docker/Podman by default | Broader ecosystem and Compose compatibility | Does not meet Apple-native default; adds daemon/VM dependency | Deferred compatibility backend |

The subprocess overhead is insignificant compared with VM boot, image build, dependency installation, and service readiness.

### Service topology

| Option | Benefits | Costs | Decision |
|---|---|---|---|
| Integrated workspace services | Fast; loopback compatibility; one VM; simple cleanup | Services and agent share a trust boundary and resources | **Selected for MVP** |
| One sibling Apple container per service | Stronger service isolation and independent lifecycle | One VM per service; higher startup/memory/network complexity | Deferred opt-in |
| Reuse host services | Avoid duplicate installation and data | Weakens isolation; host loopback bridging and ownership are complex; agent can modify host state | Not the default |

### Host lifecycle

| Option | Benefits | Costs | Decision |
|---|---|---|---|
| Daemonless DSX CLI with temporary helpers | Minimal installation and idle cost; simple ownership | No background scheduler; crash recovery must use manifests/labels | **Selected** |
| Permanent DSX host daemon | Central scheduling, monitoring, and background orchestration | More security surface, lifecycle complexity, and idle overhead | Deferred |

### Terminal interface

| Option | Benefits | Costs | Decision |
|---|---|---|---|
| Bubble Tea v2 + Huh v2 in `dsx` | Native Go stack; setup forms, dashboard composition, accessibility path; shares one binary | Adds TUI dependencies and terminal-state testing | **Selected** |
| Hand-written ANSI UI | Few dependencies | High input/rendering/accessibility risk; recreates solved terminal behavior | Rejected |
| Separate GUI/web UI | Rich visuals | Adds service, packaging, security, and lifecycle surface | Out of MVP |
| Help text only | Smallest implementation | Poor first-run discovery for a configuration-heavy security tool | Rejected |

### Workspace strategy

| Option | Benefits | Costs | Decision |
|---|---|---|---|
| Live mount | Immediate edits; simplest interactive workflow | Agent can damage the host checkout and see ignored files | Selected for one interactive `shell` |
| Guest-owned private clone from Git bundle | Independent Git metadata; excludes ignored files; supports parallel agents and branch retrieval | One VM/clone/service set per sandbox; clean host tree required; explicit fetch workflow | **Selected for autonomous `run`** |
| Host Git worktree | Efficient parallel branches and immediate host visibility | Shares host Git objects/metadata; `.git` pointer complicates isolated mounts; agents can affect shared state | Rejected as the security boundary |

## Tradeoffs and consequences

### Positive consequences

- DSX planning overhead remains small relative to VM and project startup.
- The host and Linux guest lifecycle components share one language and repository.
- Apple runtime changes are contained within one tested-version adapter.
- Projects get ordinary loopback communication between applications and databases inside each sandbox.
- Multiple agents can work on one project without sharing Git metadata, writable authentication, dependency state, or databases.
- All result operations are discoverable under `dsx git`.
- Cleanup has a deterministic owned resource graph and protects unfetched work.
- No permanent DSX daemon, Git daemon, or host container-control socket is added.
- Bare `dsx` provides guided setup and lifecycle discovery without changing the explicit command contract.

### Negative consequences

- Go maintainers must implement JSONC validation and rich configuration diagnostics without TypeScript's native type ecosystem.
- Invoking a CLI requires a self-hosted Apple silicon compatibility suite.
- Integrated services are not isolated from malicious agents or dependencies inside one sandbox.
- A standard polyglot image may be large.
- Live workspace mode cannot protect the selected repository.
- Parallel clone sandboxes duplicate VMs, dependency state, and configured databases.
- Git bundle transfer requires clean tracked input, explicit result fetching, and per-repository handling for composite workspaces.
- DSX does not resolve semantic merge conflicts between sandbox branches.
- Docker Compose and Testcontainers projects remain unsupported until a compatibility backend exists.
- The TUI adds terminal-state, resizing, accessibility, and interaction tests to the first vertical slice.

## Security considerations

### Host boundary

- Never mount or forward Apple, Docker, or Podman runtime control into a workspace.
- Never mount the complete host home directory.
- Do not expose SSH agent, GPG agent, macOS Keychain, Tailscale state, or browser profiles by default.
- Invoke host processes with structured arguments and controlled environments.
- Bind published ports to `127.0.0.1` unless the user makes an explicit trust grant; treat the runtime bind result as authoritative.
- Use restrictive temporary files for source/result Git bundles and remove them after transfer.
- Limit temporary host proxies to an active project/sandbox/run and remove them during cleanup.

### Project configuration

- Treat setup commands, process definitions, plugins, skills, hooks, and MCP servers as executable code.
- Show the effective configuration and provenance before launch.
- Require approval when the executable configuration hash changes.
- Require unattended callers to provide the exact hash through `--approve-config`; `--force` does not bypass trust.
- Never run imported host lifecycle commands silently.
- Fail closed on unsupported security-relevant fields.

### Guest boundary

- Run as a non-root user by default.
- Guest elevation grants control over the sandbox VM and every mounted resource; it must not grant host runtime control.
- Apply CPU, memory, and clone-concurrency limits.
- Use separate writable volumes for each sandbox's dependencies, service data, authentication, and sessions.
- The integrated topology intentionally does not isolate MySQL, Redis, applications, and agents inside one sandbox.
- Clone sandboxes isolate files and guest state but do not prevent source exfiltration or semantic merge conflicts.

### Credentials

- Prefer first login inside a dedicated Linux authentication volume or per-run secret injection.
- Seed a run-specific writable authentication volume for concurrent sandboxes rather than sharing one writable profile.
- Mount reusable configuration and skills read-only where supported.
- Never bake credentials into OCI images or log credential values.
- Treat OAuth tokens stored in host-resident DSX/VM volume data as sensitive plaintext at rest.
- Leapp integration is explicit and read-only at the filesystem boundary, but all mounted profiles remain readable to the workspace.

### Browser

- Keep automated browsing in a separate VM without source or credential mounts.
- Share only the owning sandbox's private application network.
- Expose Playwright through a DSX-managed MCP endpoint and inject only ephemeral harness configuration.
- Use a temporary host callback bridge for OAuth; do not reuse the daily browser profile for routine automated tests.

### Terminal UI

- Treat repository names, paths, configuration text, process labels, and runtime output as untrusted display input; strip or escape ANSI and control sequences before rendering.
- Never display secret values and mask secret input.
- Do not create configuration or runtime resources before final confirmation.
- Resource selectors update the in-memory configuration submitted to `PreviewSetup`; returning from review reconstructs the form from those in-memory choices and performs no write or runtime mutation.
- Restore terminal state on normal exit, cancellation, child-process handoff, and recoverable errors.
- Respect `NO_COLOR`, narrow terminals, resize events, and Huh's accessible mode.
- In non-interactive contexts, print help and exit rather than opening prompts.

### Cleanup

- Delete only resources with corroborating DSX ownership metadata.
- Never delete Apple runtime-owned builder state.
- Refuse deletion of unfetched sandbox results without explicit confirmation.
- Leave ambiguous resources intact and report them.
- Make cleanup idempotent and test normal exit, failure, interruption, and partial creation.
- Global authentication deletion requires an explicit flag and confirmation.

## Assumptions

- The target host has Apple silicon and macOS 26 or newer.
- Apple `container` versions `>=1.2.2 <1.3.0` provide the required CLI, ARM64 image, mount, network, volume, exec, inspection, and loopback publication behavior.
- Rosetta and amd64-only project dependencies are outside the MVP.
- Go can provide the required PTY behavior or use a small maintained PTY dependency without a DSX daemon.
- Supported projects can execute on Linux ARM64 and clone from Git bundles.
- Most local application and infrastructure processes can coexist in one sandbox VM.
- Project maintainers will provide explicit configuration for non-obvious lifecycle behavior and composite repository membership.
- Browser isolation is worth one additional VM per browser-enabled sandbox because it handles untrusted content.
- Per-harness authentication can be copied into independent writable sandbox volumes without mounting complete host dotfile trees.
- Atomic manifests plus runtime labels are sufficient before background scheduling or multi-host operation exists.
- Interactive users have a terminal compatible with Bubble Tea, or can use the accessible/plain CLI path.

## Implementation outline

### Vertical slice 1: lifecycle and live-mount proof

- Implement `dsx doctor`, `dsx inspect`, `dsx shell`, `dsx ls`, `dsx stop`, and `dsx clean`.
- Add bare-`dsx` state selection, the Huh setup flow, launcher, and minimal Bubble Tea dashboard over the same application services.
- Verify non-TTY help fallback, side-effect-free cancellation, configuration review/approval, ANSI sanitization, `NO_COLOR`, resize/narrow layouts, accessible mode, and terminal restoration around interactive child processes.
- Validate macOS, Apple runtime version, system service, and builder state.
- Start a pinned standard image against `course-intelligence-agency`.
- Mount the project, attach a terminal, forward signals, and prove complete cleanup.
- Verify bidirectional file propagation and Vite/webpack/Next file watching and HMR.

### Vertical slice 2: guest processes and configuration

- Bind-mount `dsx-guest`, implement `/run/dsx/control.sock`, and exercise `container exec` JSON control operations.
- Add `.dsx/config.jsonc` parsing, validation, provenance, interactive approval, and `--approve-config`.
- Add process health checks, prefixed logs, integrated services, and failure semantics.
- Exercise the composite `devenv` workspace with MySQL, Redis, Caddy, and selected application processes.

### Vertical slice 3: named clone sandboxes and harnesses

- Add OMP, Codex, Claude Code, and OpenCode adapters.
- Add named authentication profiles, per-sandbox writable copies, and read-only reusable configuration.
- Add multiple concurrent `dsx run --name` sandboxes.
- Add bundle-based source cloning and `dsx git status`, `dsx git diff`, `dsx git fetch`, and `dsx git apply`.
- Verify two same-project sandboxes share no Git metadata, writable auth, service state, or fixed host ports.

### Vertical slice 4: browser and host integration

- Add isolated Playwright MCP browser support and ephemeral harness injection.
- Add host URL opening and OAuth callback forwarding.
- Add explicit Leapp and host-network/Tailscale proxy grants.

Each slice must include lifecycle failure and cleanup tests before adding the next resource type. Unit and adapter command-generation tests run in ordinary CI; VM, PTY, mount, network, cleanup, and version-compatibility tests run on self-hosted Apple silicon.

## What would change this decision

Reconsider Go or the CLI boundary if:

- Apple publishes a stable, supported high-level API that materially improves security or functionality over the CLI.
- Required runtime behavior cannot be observed or controlled reliably through machine-readable CLI operations.
- A required Network Extension, Keychain, or Virtualization feature demands a privileged Swift component.
- Go PTY behavior proves unreliable for the supported harnesses.
- Bun/TypeScript reduces implementation complexity enough to outweigh the larger runtime and systems-code risk.
- Real project measurements show one integrated VM per sandbox cannot meet startup, memory, or service-isolation requirements.
- Git bundle transfer or independent per-sandbox authentication cannot support the required harness workflows safely.
- Terminal behavior or accessibility requirements cannot be met reliably with Bubble Tea/Huh without compromising the explicit CLI.
- A material share of target projects requires Docker Engine APIs.

## Revisit

Revisit this ADR after vertical slice 2 passes lifecycle tests for both reference workspaces, and again after two named clone sandboxes pass slice 3 isolation and result-transfer tests. Review measured startup time, memory, cleanup reliability, Apple CLI compatibility, PTY correctness, file watching, integrated-service behavior, and Git transfer safety.
