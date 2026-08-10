# DSX Product Requirements Document

- **Status:** Draft
- **Version:** 0.3
- **Date:** 2026-08-09
- **Audience:** DSX users and maintainers

Current command, configuration, security, and external-gate behavior is documented in the [DSX user and operator guide](./manual/user-guide.md). This PRD defines the product contract; the guide distinguishes implemented behavior from release evidence that is still blocked.

## 1. Product summary

DSX is a fast, local, command-line development sandbox for macOS on Apple silicon. From an existing project directory, a developer can start an isolated Linux workspace, run supported coding-agent harnesses, run the project's application and local infrastructure, expose selected ports, test through an isolated browser, and remove all project resources with one command.

DSX uses Apple's `container` runtime. It does not replace the project's build system or infer arbitrary lifecycle commands.

## 2. User problem

A developer may already have source code, dependencies, local processes, credentials, and several unrelated projects on the Mac. Running coding agents directly on the host gives those agents access to more files, credentials, processes, and control sockets than the project requires. Existing container workflows also vary significantly between monorepos.

The user needs a tool that:

- Starts quickly from the current project directory.
- Keeps host files and credentials outside the granted project boundary.
- Supports TypeScript, Python, Java, PHP, and polyglot monorepos.
- Runs OMP, Codex, Claude Code, or OpenCode without separate environment setup.
- Can run application processes and infrastructure such as MySQL and Redis internally.
- Preserves selected agent authentication between runs.
- Provides browser access and loopback-only ports.
- Reliably removes every resource DSX created for a project.

## 3. Product principles

1. **One useful command:** bare `dsx` opens a context-aware setup or project dashboard; explicit `dsx shell` and `dsx run` remain scriptable.
2. **Fast by reuse:** reuse pulled images, build layers, authentication, and explicitly persistent state.
3. **Explicit over clever:** inspect existing project declarations, but do not silently invent or execute lifecycle commands.
4. **Project-scoped authority:** expose only the selected workspace, credentials, network paths, and ports.
5. **Owned cleanup:** DSX deletes only resources it can prove it owns.
6. **No DSX daemon:** temporary DSX helpers exist only while a workspace needs them; Apple `container` system services remain a prerequisite.

## 4. Target users

### Primary user

A macOS developer using Apple silicon who wants to run one or more coding-agent harnesses against an existing local software project.

### Project maintainer

A developer who commits `.dsx/config.jsonc` so other contributors can start the same environment without reconstructing project setup knowledge.

## 5. Core user journeys

### 5.1 Existing project with a Dev Container definition

```bash
cd /Volumes/Dev/work/course-intelligence-agency
dsx inspect --mode clone --sandbox fix-test --agent codex --browser
dsx run --name fix-test --agent codex --browser --approve-config 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef -- "fix the failing test"
dsx git diff fix-test
dsx git fetch fix-test
dsx clean --name fix-test
```

The approval hash above is illustrative; copy the exact hash printed by the preceding inspection.

Expected behavior:

1. `dsx inspect` reads supported fields from `.devcontainer/devcontainer.json`, Dockerfiles, lockfiles, and `.dsx/config.jsonc` if present. It prints the effective plan and executes nothing.
2. `dsx run` creates a named isolated project workspace, reuses or builds the required image, attaches run-specific writable authentication state, installs Linux dependencies using approved lifecycle commands, starts configured processes, and launches Codex.
3. The optional browser runs separately and can reach the application over the private project network.
4. Agent changes remain on the sandbox branch until `dsx git fetch` imports it into a host remote-tracking ref.
5. `dsx clean --name fix-test` removes that sandbox while preserving global authentication unless explicitly purged.

Existing host dependencies and processes are not modified or adopted automatically.

### 5.2 Composite workspace containing multiple repositories

```bash
cd /Volumes/Dev/work/devenv
dsx inspect
dsx init                 # first project setup only
dsx shell --agent claude
dsx stop
dsx clean
```

Expected behavior:

1. `dsx inspect` identifies multiple Git roots, `devenv.nix`, process definitions, and service definitions. It reports what DSX understands and what requires explicit configuration.
2. `dsx init` creates `.dsx/config.jsonc` from safe, reviewable suggestions. It does not translate or execute arbitrary Nix expressions.
3. The committed configuration declares workspace member repositories, the Linux image/build, setup commands, processes, services, health checks, and ports.
4. `dsx shell` starts one integrated Linux workspace. Laravel, frontend processes, MySQL, Redis, Caddy, and the selected agent may run as processes inside that workspace.
5. Container-local services communicate over `127.0.0.1`; only user-facing ports are published to macOS.
6. `dsx stop` preserves configured workspace state. `dsx clean` deletes it.

### 5.3 Switching projects

A user may run DSX from a second directory while another DSX project is active. Every project receives a stable project ID derived from its canonical workspace root and a unique run ID. Containers, volumes, networks, ports, clones, and state from one project must not be reused by another project unless the resource is explicitly global, such as an agent authentication profile.

### 5.4 Parallel agents in one project

```bash
dsx inspect --mode clone --sandbox api --agent codex
dsx run --name api --agent codex --approve-config 1111111111111111111111111111111111111111111111111111111111111111 -- "implement the API"
dsx inspect --mode clone --sandbox tests --agent claude
dsx run --name tests --agent claude --approve-config 2222222222222222222222222222222222222222222222222222222222222222 -- "add API tests"
dsx ls
dsx git fetch api
dsx git fetch tests
```

The hashes above are illustrative; inspect each named sandbox/agent plan and pass its exact hash.

Each named run receives an independent Apple container/VM, guest-owned Git clone, branch, dependency state, service state, writable authentication copy, and dynamic host ports. One live-mounted interactive workspace is allowed per project; multiple named clone sandboxes may run concurrently. DSX provides isolation and result transfer, but does not schedule, coordinate, or merge agent work.

### 5.5 Bare-command setup and project dashboard

From a terminal in a project directory, the user may run:

```bash
dsx
```

DSX resolves the current project before choosing a screen:

- No `.dsx/config.jsonc` and no DSX resources: open the setup wizard.
- Configuration exists but no resources: open a launcher for live workspace or named clone creation.
- Existing resources: open the project dashboard with sandbox status and safe lifecycle/Git actions.
- No interactive terminal: print command help and exit without prompting or changing state.

The setup wizard presents detected project facts, optional capabilities, and the complete effective plan and trust grants. It writes `.dsx/config.jsonc` or creates resources only after final confirmation. `dsx init` opens the same wizard directly.

## 6. Command surface

| Command | User outcome |
|---|---|
| `dsx` | Open the context-aware setup wizard, project launcher, or sandbox dashboard when attached to a terminal; otherwise print help. |
| `dsx inspect` | Show the effective image, workspace, commands, processes, services, mounts, credentials, network access, ports, and configuration sources. Make no changes. |
| `dsx init` | Generate a reviewable `.dsx/config.jsonc` when an existing declaration cannot fully describe the workspace. |
| `dsx start --approve-config <hash>` | Start the approved live workspace without attaching. |
| `dsx shell [--agent <name>] [--profile <name>] [--approve-config <hash>]` | Start or attach to the single interactive integrated workspace. Default to live workspace mode. |
| `dsx run --name <name> --agent <agent> [--profile <name>] [--browser] --approve-config <hash> -- <prompt>` | Create or resume a named private-clone sandbox and run one agent task. |
| `dsx list` / `dsx ls` | List DSX sandboxes, project ownership, lifecycle state, and published ports. |
| `dsx status [--format text|json]` | Show final URLs and configured-process state for the live workspace. |
| `dsx logs [--format text|json] <process>` | Return bounded retained output for one configured live-workspace process; do not follow. |
| `dsx git status <name> [--repo <member>]` | Show the source ref, result branch, dirty state, host fingerprint, and fetch state for clone sandboxes. |
| `dsx git diff <name> [--repo <member>]` | Show safely rendered changes produced in a private clone. |
| `dsx git fetch <name> [--repo <member>]` | Import sandbox commits through a verified Git bundle into a host remote-tracking ref. |
| `dsx git apply <name> [--repo <member>]` | Apply a sandbox result as a guarded squashed working-tree change. |
| `dsx login --agent <agent> --profile <name> --root <path> --approve-config <hash>` | Run the selected provider's explicit interactive login and promote allowlisted credential artifacts. |
| `dsx stop [--name <name>]` | Stop one sandbox, or the current project's live workspace, while retaining explicitly persistent state. |
| `dsx clean [--name <name>]` | Remove one named sandbox or all proven DSX-owned resources for the current project. |
| `dsx clean --all` | Remove proven DSX-owned resources for every project after confirmation. |
| `dsx clean --purge-auth --agent <agent> [--profile <name>]` | Also remove the selected persisted authentication profile after cleanup; active copies block purge. |
| `dsx doctor [--require-builder]` | Read-only validation of supported host architecture/OS, exact Apple CLI/API-server pair, service, builder, and compatibility allowlist. |
| `dsx version [--json]` / `dsx --version [--json]` | Print build, guest-helper digest, and pinned image metadata. |

Destructive commands require confirmation in an interactive terminal. `--force` bypasses only destructive cleanup confirmation; it never bypasses executable-configuration approval.

## 7. Functional requirements

### R1. Host and runtime

- DSX must require macOS 26 or newer on Apple silicon; no macOS 15 degraded mode is included in the MVP.
- DSX must initially support Apple `container` versions `>=1.2.2 <1.3.0` and reject untested versions until the compatibility suite passes.
- DSX must verify that the Apple `container` CLI and its required system services are usable.
- DSX must invoke the installed runtime rather than exposing its control socket or service API inside a workspace.
- DSX must fail with an actionable error when a required runtime capability is unavailable.

### R2. Project identity and isolation

- DSX must assign a stable project ID, a user-visible sandbox name, and a unique run ID.
- Multiple named clone sandboxes may run concurrently for one project; only one live-mounted workspace may exist per project, and live and managed clone modes do not coexist in the MVP.
- Every resource must carry DSX ownership, project ID, sandbox name, run ID, resource type, and creation-time metadata where the runtime supports labels.
- A workspace must not receive the host home directory, unrelated repositories, container control sockets, SSH agent, GPG agent, or macOS Keychain by default.
- CPU, memory, and per-project clone-sandbox concurrency limits must be configurable.

### R3. Workspace modes

- `dsx shell` must support one live read-write workspace per project for fast interactive use.
- `dsx run` must use a named private clone by default for autonomous changes.
- Clone mode must require a clean tracked host working tree for the MVP and must warn that ignored and untracked files are excluded.
- DSX must create the clone in a guest-owned volume from a Git bundle, not a persistent host workspace mount.
- Local clone creation must prohibit shared object hardlinks. Each sandbox must have independent Git metadata and a branch derived from the selected source ref.
- Composite workspaces must declare each participating repository. Clone mode must reproduce each selected repository at its configured relative path.
- Existing macOS dependency artifacts must remain untouched. Linux dependency directories and caches must use sandbox- or project-scoped volumes.
- Removing a sandbox with commits that have not been fetched or otherwise exported must fail unless the user explicitly forces deletion.

### R4. Images and dependencies

- DSX must provide a versioned standard Linux ARM64 image containing common development tools and supported agent harnesses.
- Project-specific system dependencies must be added through a project image or explicit build definition.
- Image builds must use OCI layers and content-addressed caching.
- Setup commands must run only when declared by `.dsx/config.jsonc` or imported from an approved supported field.
- A changed configuration or image input must invalidate the relevant cached setup state.

### R5. Integrated processes and services

- A workspace must support multiple concurrent processes.
- The selected agent, application processes, MySQL, Redis, Caddy, workers, and other configured services may run inside one integrated container.
- Container-local processes must be able to communicate through loopback.
- Each process may declare environment variables, dependencies, health checks, and log identity.
- DSX-managed process output must be multiplexed to container stdout/stderr with a process-name prefix.
- DSX must wait for required health checks before launching a dependent agent or browser task.
- DSX does not automatically restart failed processes in the MVP; a failed required process makes the sandbox failed. A configured project process manager may implement its own restart policy.
- Sibling service containers are not required for the MVP.

### R6. Agent harnesses

- The MVP must support OMP, Codex, Claude Code, and OpenCode.
- Multiple harness CLIs may be installed in one workspace image.
- The user must select which harness DSX launches for a task.
- DSX must support interactive invocation, one-shot invocation, output streaming, cancellation, exit-code propagation, and terminal resize.
- Multiple named clone sandboxes may run different harnesses concurrently against the same source project.
- Concurrent editing harnesses inside one live workspace require an explicit warning; DSX does not coordinate their changes.

### R7. Authentication and reusable configuration

- Authentication must use dedicated volumes or per-run environment injection, never a mount of the complete host home directory.
- Agent authentication must be separable by harness and named profile.
- Each concurrent sandbox must receive its own writable authentication/session volume; DSX must not mount one writable profile volume into multiple sandboxes concurrently.
- Reusable skills and reviewed harness configuration may be shared read-only.
- Authentication may persist across container recreation when the user selects a persistent profile.
- `dsx clean` must preserve global authentication by default; `--purge-auth` must remove it explicitly.

### R8. AWS and Leapp

- AWS integration must be opt-in.
- Leapp mode must mount the required AWS configuration directory read-only and preserve atomic credential rotation.
- The user must use standard `AWS_PROFILE` and AWS CLI behavior inside the workspace; DSX must not introduce a parallel AWS profile command.
- DSX must visibly warn that profile selection is convenience, not credential isolation: an agent that can read the mounted AWS directory can potentially read every profile it contains.
- DSX must never print credential values.

### R9. Networking

- Public internet access must support dependency installation, agent APIs, Git, and AWS APIs.
- Host or private-network access must be an explicit trust grant displayed by `dsx inspect`.
- DSX must not mount Tailscale identity or state into the workspace.
- When direct guest routing cannot reach an approved host-only network, DSX may start a temporary host proxy limited to the active project network.
- Host proxies must stop with the workspace and be removed by cleanup.

### R10. Ports

- Published ports must bind to `127.0.0.1` by default.
- For unspecified host ports, DSX must request dynamic allocation where the runtime supports it.
- For fixed ports, the runtime bind result is authoritative; a preflight conflict check may improve diagnostics but is not a safety guarantee.
- The user must explicitly request non-loopback binding.
- DSX must display the final host URLs and port mappings per sandbox.
- Port forwarding must be removed during stop or cleanup as appropriate.

### R11. Browser support

- Automated browser testing must use a separate disposable browser container/VM by default.
- The browser must share only the private project network with its owning sandbox.
- The browser must not receive the project repository, agent authentication, AWS credentials, or host home mounts.
- The browser must expose a DSX-managed Playwright MCP endpoint over the private network.
- DSX must inject ephemeral harness configuration for that endpoint without modifying global harness configuration.
- OAuth and interactive agent login URLs must open in the macOS browser through a temporary callback bridge.

### R12. Configuration and trust

- Project configuration must live at `.dsx/config.jsonc`.
- Configuration precedence must be: CLI flags, project DSX configuration, explicitly imported supported configuration, standard defaults.
- `dsx inspect` must show each effective value and its source and be able to print the executable configuration hash.
- Host-side commands and unsupported Dev Container fields must never execute silently.
- DSX must require approval when executable project configuration changes since the last approved hash.
- Non-interactive execution must supply the exact expected hash through `--approve-config`; `--force` must not bypass this check.
- Unsupported configuration must fail visibly rather than being ignored.

### R13. Lifecycle and cleanup

- `dsx stop` must stop one selected sandbox, or the live workspace, while preserving explicitly persistent state.
- `dsx clean --name <name>` must remove every DSX-owned resource for that sandbox.
- `dsx clean` without a name must remove all DSX-owned resources for the current project: workspace containers, browsers, helper processes, networks, published ports, proxies, private clones, project caches, dependency volumes, service data including databases, logs, temporary files, and lifecycle manifests.
- Cleanup must refuse to delete unfetched clone results unless the user explicitly confirms loss.
- Cleanup must handle normal completion, agent failure, Ctrl-C, terminal closure, partial startup, and stale resources after a crash.
- Cleanup must be idempotent.
- Cleanup must never delete an unrelated Apple container resource, Apple runtime-owned builder, or host process/data directory that DSX did not create.
- Global authentication must survive ordinary cleanup and require explicit purge.

### R14. Performance

- A warm configured workspace must avoid image rebuilds and dependency reinstallations when relevant inputs have not changed.
- Warm `dsx inspect` should complete within 500 ms at p95 and must not start a VM.
- A cached empty `dsx shell` should reach a prompt within 3 seconds at p95, excluding project setup and service readiness.
- DSX planning before runtime invocation should complete within 250 ms at p95.
- Ordinary `dsx clean` should complete within 5 seconds at p95 when the runtime is responsive.
- DSX host and guest helpers should use no more than 100 MiB combined, excluding Apple runtime and project processes.
- DSX must not require a permanent DSX daemon.
- Browser and host proxy resources must start only when requested.
- Startup timing must distinguish Apple builder initialization, image pull/build, workspace boot, setup, service readiness, and agent launch.

### R15. Terminal user interface

- Bare `dsx` must select setup, launcher, or dashboard state from the current project's configuration and DSX-owned resources.
- `dsx init` must open the same setup flow directly.
- The TUI must be a presentation adapter over the same application services used by explicit CLI commands; it must not implement separate lifecycle state transitions.
- The setup flow must show detected facts, selected capabilities, executable commands, mounts, credentials, network grants, ports, and the resulting configuration hash before confirmation.
- No project configuration or runtime resource may be created before final confirmation.
- The dashboard must support listing, creating, attaching, starting, stopping, cleaning, and invoking `dsx git status`, `dsx git diff`, and `dsx git fetch`.
- The TUI must leave its alternate screen before handing control to an interactive agent or shell and restore it when that process exits.
- Ctrl-C before confirmation must leave no changes; interruption during creation must use the normal rollback and cleanup path.
- When stdin or stdout is not an interactive terminal, bare `dsx` must print help and exit without opening prompts.
- The TUI must respect `NO_COLOR`, terminal resizing, narrow layouts, and an accessible form mode.

## 8. Security considerations

### Trust boundaries

1. **Host control plane:** DSX owns runtime operations and temporary host integration.
2. **Workspace:** the agent may control every resource mounted into or reachable from the integrated workspace.
3. **Browser:** untrusted web content is isolated from source and credentials.
4. **External systems:** agent providers, package registries, Git hosts, AWS, and explicitly approved private networks.

### Required controls

- Run the workspace as a non-root user by default. Guest elevation, if enabled, grants authority only inside the workspace VM.
- Drop unnecessary capabilities and do not expose host runtime control.
- Pass process arguments as structured arrays; do not construct host shell command strings from project input.
- Treat setup commands, hooks, agent skills, plugins, MCP servers, and harness configuration as executable code.
- Keep secrets out of images, logs, generated configuration, and command-line arguments where possible.
- Separate credentials, reusable configuration, sessions, caches, and project source into distinct mounts or volumes.
- Use loopback-only host publication and explicit private-network grants.
- Make every host trust grant visible before launch.
- Make cleanup ownership fail closed: ambiguous resources are reported, not deleted.
- Clone-mode source transfer and result import must use generated temporary files with restrictive permissions and remove them after use.
- Repository names, paths, configuration text, process labels, and runtime output must be treated as untrusted terminal content; DSX must strip or escape ANSI and control sequences before TUI rendering.
- The TUI must never display secret values and must mask any secret input.

### Residual risks

- An agent can modify or delete any live-mounted repository content.
- An agent can read and exfiltrate any secret present in the workspace, including repository `.env` files and explicitly mounted AWS credentials.
- Persisted provider OAuth tokens remain sensitive plaintext inside host-resident DSX/VM volume data.
- A malicious dependency or setup command has the same workspace authority as the agent.
- Internet access permits source exfiltration unless a later network policy restricts destinations.
- MicroVM isolation protects the host boundary but does not protect resources deliberately mounted or proxied into the workspace.
- Multiple editing agents in one live workspace can conflict or corrupt each other's work; named clone sandboxes isolate files but do not resolve semantic merge conflicts.

## 9. Assumptions

- The user has Apple silicon and macOS 26 or newer.
- Apple `container` is installed separately and can pull OCI images.
- Projects and their required images can run on Linux ARM64.
- Rosetta and amd64-only dependencies are outside the MVP.
- Project maintainers can declare non-obvious setup and service commands.
- An integrated container is sufficient for each sandbox's MVP application and infrastructure processes.
- Loopback publication satisfies normal local development access.
- Agent-provider authentication mechanisms permit per-sandbox Linux volumes or per-run injection.
- Leapp continues to update standard AWS credential files atomically.
- Host browser automation does not require browser profile reuse for the default isolated testing path.
- The user's interactive terminal supports the minimum capabilities required by Bubble Tea, or DSX can fall back to accessible/plain output.

## 10. What DSX does not do

The MVP does not:

- Replace Nix, pnpm, uv, Maven, Gradle, Composer, or a project's process manager.
- Fully interpret arbitrary `devenv.nix`, shell scripts, Docker Compose, or the complete Dev Container specification.
- Automatically execute inferred install, migration, seed, or startup commands.
- Provide Docker Engine APIs, Docker Compose, Testcontainers, nested containers, Kubernetes, Rosetta, or amd64 emulation.
- Transparently inherit every macOS VPN route or arbitrary non-HTTP network protocol.
- Prevent an agent from exfiltrating credentials or source that the user explicitly grants to it.
- Isolate one process from another inside an integrated sandbox.
- Schedule tasks, coordinate agent prompts, resolve semantic conflicts, or merge parallel results automatically.
- Provide a graphical desktop/web interface, IDE, cloud execution service, or remote multi-host scheduler; the local terminal UI is part of the MVP.
- Migrate users away from Leapp or replace standard AWS profile behavior.
- Delete host dependencies, host databases, host processes, Apple runtime-owned builders, or non-DSX container resources.

## 11. MVP acceptance criteria

1. Bare `dsx` in an unconfigured project opens the setup wizard; cancelling before confirmation creates no file or runtime resource.
2. Bare `dsx` in a configured project with no resources opens the launcher, and with existing resources opens the project dashboard.
3. Bare `dsx` without an interactive terminal prints help and exits without prompting.
4. From `course-intelligence-agency`, one command starts a Linux workspace and launches each supported harness.
5. The workspace builds and runs configured Node, Python, Java, PHP, and project processes.
6. Authentication survives workspace recreation when configured.
7. Global skills are available without mounting the full host home directory.
8. An isolated Playwright browser can exercise a published application through the injected MCP endpoint without source or credential mounts.
9. Published ports listen only on `127.0.0.1` by default.
10. From the composite `devenv` workspace, configured repositories and integrated MySQL, Redis, Caddy, and application processes start and communicate internally.
11. Two named clone sandboxes for the same project run concurrently without sharing Git metadata, writable authentication state, services, or fixed host ports.
12. A second project can run concurrently without resource-name, state, network, or cleanup collision.
13. `dsx git fetch` imports new, deleted, renamed, binary, committed, and final uncommitted agent changes into a sandbox-specific host ref.
14. Live-mount file changes propagate in both directions and configured Vite/webpack/Next file watching and HMR work.
15. `dsx inspect` lists every mount, credential profile, host-network grant, process, service, and port before execution.
16. `dsx clean --name` removes one sandbox; `dsx clean` removes all current-project resources while preserving unrelated projects and global authentication.
17. Cleanup refuses to destroy unfetched results without explicit confirmation.
18. `dsx clean --purge-auth` removes selected persisted authentication.
19. Ctrl-C and partial startup leave either no ephemeral resources or resources discoverable and removable by `dsx clean`.

## 12. Success measures

- Warm workspace startup is dominated by Apple VM boot and service readiness, not DSX planning overhead.
- No unrelated host resource is deleted during the MVP acceptance suite.
- All MVP project resources are removed by one successful `dsx clean` invocation.
- A configured project requires no host toolchain installation beyond DSX and Apple `container`.
- A returning user can switch agent harnesses without rebuilding the project environment.
