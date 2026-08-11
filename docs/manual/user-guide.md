# DSX user and operator guide

This guide describes the current MVP command and configuration contracts. It is not evidence that an optional integration or release gate has passed. See [External evidence and release gates](#external-evidence-and-release-gates) for the exact blocked items.

New to DSX? Start with [Getting started with DSX](./getting-started.md) for installation, first-project, live workspace, named clone, authentication, browser, and cleanup walkthroughs.

## 1. Requirements and setup

DSX runs only on Apple-silicon (`arm64`) Macs with macOS 26 or newer. Install Apple `container` separately. The current compatibility allowlist admits only the exact pair `container` CLI 1.2.2 and API server 1.2.2; versions below 1.2.2, 1.3.0 or newer, mismatched CLI/server versions, and untested 1.2.x patches fail before resource creation.

A DSX installation consists of the Darwin/arm64 `dsx` binary and the static Linux/arm64 `dsx-guest` binary beside it. Do not copy only `dsx`: the host stages and digest-verifies the adjacent guest helper for each workspace. No registry package or signed/notarized public installer is available until the release gates below pass.

After installation, run the read-only checks:

```console
$ dsx version
$ dsx doctor
$ dsx doctor --require-builder
```

`doctor` checks the host OS and architecture, exact CLI/API-server pair, Apple system service, compatibility allowlist, and builder health. Without `--require-builder`, an unhealthy builder is a warning; with it, the check fails. DSX may tell an operator to run Apple-native `container system start` or `container builder start`; DSX does not install, uninstall, stop, prune, or delete the Apple runtime or its builder.

## 2. Bare `dsx`, setup, and non-TTY behavior

In a terminal, bare `dsx` resolves the canonical current project and routes to the setup wizard, launcher, or dashboard according to configuration and owned resources. `dsx init [--root PATH]` opens the setup flow directly. Its review gives each topic a separate color-coded view for detected facts, the effective plan, executable commands, mounts, credential and network grants, ports, provenance, and the executable hash. The shared TUI column, stepper, panels, status, and footer controls use consistent centered alignment and outer padding. Page position, section continuation, and navigation cues remain visible before confirmation. Cancelling before final confirmation must not write configuration or create runtime resources. After confirmation, a development build without published image metadata builds and verifies the embedded DSX Standard image; release builds continue to use their published digest-pinned standard image.

Setup includes explicit CPU and memory selectors. New sandbox configurations default to 4 CPUs and 6 GiB. The review's `b` action returns to the first environment screen and preserves all in-memory choices; it does not write configuration or mutate runtime resources.

After final confirmation, setup performs a read-only `container system status --format json` preflight before writing configuration or approval state. A missing Apple container CLI or a service state other than `running` stops setup without persistence; start a stopped service with `container system start`, then retry.

If either stdin or stdout is not a TTY, bare `dsx` prints command help and exits without prompting or changing state. `dsx init` instead fails because setup is interactive. Use explicit commands and their text/JSON output in automation. Set `DSX_ACCESSIBLE=1` for the accessible TUI form mode; `NO_COLOR` is also respected.

By default, setup writes `~/.dsx/projects/<project-name>-<project-id>/config.jsonc`. A repository `.dsx/config.jsonc` is supported as an explicit shared alternative. DSX requires exactly one location and refuses to continue if both exist. Configuration precedence is CLI flag, the active DSX configuration, explicitly selected supported import field, then default. Unknown fields and unsupported security-relevant declarations fail visibly; inferred lifecycle commands are not executed automatically.

## 3. Inspect and approve before mutation

`inspect` is read-only:

```console
$ dsx inspect [--format text|json] [--root PATH] [--mode live|clone] [--sandbox NAME] [--agent NAME]
```

It reports detected project facts and, when configuration is complete, the effective image, workspace members, commands, processes and health checks, mounts and volumes, auth profile, browser choice, network grants, ports, resources, provenance, and `executable_hash`. Clone inspection requires `--mode clone --sandbox NAME`, where `NAME` is not `main`.

Before a noninteractive mutation, copy the exact 64-character lowercase hash printed as `Executable hash:` (text) or `plan.executable_hash` (JSON) and pass it through `--approve-config`. A stale or wrong hash fails. `--force` never bypasses this trust check.

```console
$ dsx inspect --mode clone --sandbox fix-test --agent codex
$ dsx run --name fix-test --agent codex --approve-config 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef -- "fix the failing test"
```

The hash above is syntactically valid documentation data, not approval for a real project. Replace it with the exact hash from the immediately preceding inspection.

## 4. Live workspace and named clone sandboxes

The modes deliberately do not coexist for one project:

- `shell` uses the one live workspace named `main` and mounts selected host repositories read/write. Changes propagate immediately in both directions. The sandbox can modify or delete those host files.
- `run --name NAME` uses a guest-owned private clone created from restrictive Git bundles. It does not persistently mount host source. The host tracked tree must be clean; ignored and untracked host files are excluded. Each clone gets independent Git metadata, dependency state, services, dynamic ports, and writable auth state.

Multiple named clones may run concurrently, subject to `resources.maxConcurrentClones`; DSX does not schedule prompts or merge results. Multiple editing agents in the live workspace can conflict and require an explicit warning.

### Start, shell, and run

```console
$ dsx start [--root PATH] --approve-config HASH
$ dsx shell [--root PATH] [--approve-config HASH] [-- command args...]
$ dsx shell [--root PATH] [--approve-config HASH] --agent omp|codex|claude|opencode [--profile NAME]
$ dsx run --name NAME --agent omp|codex|claude|opencode [--profile NAME] [--browser] --approve-config HASH -- "one prompt"
```

`start` starts the approved live workspace without attaching. `shell` starts or attaches to it. A direct command must follow `--`; `shell --agent` is interactive and cannot also take a direct command. `run` requires exactly one non-empty prompt after `--` and creates or resumes a named clone in the current project. Interactive child exit status, signals, terminal resize, and cancellation are propagated.

Bare `dsx` keeps these operations on the same application services. After setup, one project screen reports Apple Container status and the live workspace state, then offers exactly one primary action: start the container system, create and open the first workspace, start and open a stopped workspace, or attach to a running workspace. The create and start actions in this interactive screen enter the workspace shell after it is ready; the explicit `dsx start` command remains start-only. **More options** contains only applicable isolated-clone, stop, clean, and named-clone Git operations. Creating a configured project's first workspace still reviews the complete current plan and requires final confirmation; the approval transaction does not rewrite the committed configuration. Creating an isolated clone collects the clone name, harness, authentication profile, one-shot prompt, and browser choice, renders the exact clone plan and authority before approval, then starts the harness only after confirmation.

### List, status, logs, stop, and clean

```console
$ dsx list [--root PATH] [--format text|json]
$ dsx ls [--root PATH] [--format text|json]
$ dsx status [--root PATH] [--format text|json]
$ dsx logs [--root PATH] [--format text|json] PROCESS
$ dsx stop [--root PATH] [--name NAME]
$ dsx clean [--root PATH] [--name NAME] [--force] [--discard-unfetched]
$ dsx clean --all [--force] [--discard-unfetched]
```

`list`/`ls` lists owned sandboxes and published ports. `status` reports URLs and configured process state for the current live workspace. `logs` returns the bounded retained log for exactly one configured live-workspace process; it is not a follow mode and currently has no named-clone selector. `stop` without `--name` selects the live workspace; `--name main` is rejected. Stop retains explicitly persistent state. Clean without `--name` removes all proven DSX-owned resources for the current project; `--name` removes one clone; `--all` spans every DSX project.

Clean requires a TTY confirmation unless `--force` is present. `--force` bypasses only that confirmation. It does not bypass config approval, ownership proof, or the unfetched-result guard. Ambiguous resources are retained and reported rather than guessed at. Ordinary clean preserves global authentication.

## 5. Clone result recovery and Git transfer

All Git commands require a named clone. For a composite workspace, omit `--repo` to operate on every member or select one exact configured member.

```console
$ dsx git status NAME [--repo MEMBER] [--root PATH] [--format text|json]
$ dsx git diff NAME [--repo MEMBER] [--root PATH] [--format text|json]
$ dsx git fetch NAME [--repo MEMBER] [--root PATH] [--format text|json]
$ dsx git apply NAME [--repo MEMBER] [--root PATH] [--format text|json]
```

`status` shows source/result commits, branch, tracked-host fingerprint, untracked/ignored warnings, and fetched state. `diff` safely renders text patches, omits binary content, and truncates output above its bound. `fetch` finalizes changes and imports a verified result bundle to a sandbox-specific host remote-tracking ref; it does not merge. `apply` safety-checks the host fingerprint and ref collisions, then applies the result as a squashed working-tree change. A composite apply fails without partial host mutation.

Before cleanup, run `git status`, then preserve each result with `git fetch` or `git apply`. If the guest is unavailable or any member has an uncaptured/unfetched result, cleanup fails closed. Recover or restart the sandbox and fetch again. Only when loss is intentional may an operator combine `--discard-unfetched` with the normal interactive confirmation or `--force`; this permanently discards the guarded result.

## 6. Authentication profiles

Declare profiles in `.dsx/config.jsonc` and select one with `--profile`. `persistence: "global"` survives ordinary project cleanup; `persistence: "sandbox"` is scoped to the project and sandbox. A persistent seed is never writable-mounted into a sandbox: every login/run receives an independent writable copy, and successful updates are promoted with compare-and-swap semantics.

Login is always explicit, interactive, and tied to an approved plan:

```console
$ dsx login --agent omp|codex|claude|opencode --profile NAME --root PATH --approve-config HASH
```

Normal `shell`/`run` never initiates provider login. A successful login promotes only the adapter's allowlisted credential artifacts. Concurrent refreshes never overwrite a changed seed: DSX reports a conflict and preserves a candidate rather than attempting a generic secret merge. Stop active copies and resolve by performing a fresh explicit login; no public candidate-merge command exists.

There is currently no public `dsx auth import` command. The internal repository can validate/import allowlisted portable credential artifacts, but the CLI deliberately does not claim ambient host-auth import. In particular, arbitrary OMP databases and macOS Keychain-backed Claude credentials are not portable imports. OMP seed/promotion requires a closed-process `agent.db` plus `agent.db-wal` snapshot; this remains an external harness experiment gate.

Purge selected persisted authentication only as an explicit clean option:

```console
$ dsx clean --purge-auth --agent omp|codex|claude|opencode [--profile NAME] [--root PATH] [--force]
```

The default profile is `default`. Purge occurs after resource cleanup and refuses while the selected profile has active run copies. It removes only the exact DSX-managed harness/profile; ordinary clean preserves it.

## 7. Browser, Leapp, private relay, and ports

### Isolated browser

`browser.enabled: true` or `dsx run --browser` requests a separate disposable browser VM on only the owning sandbox's private network. The CLI flag is enable-only: it cannot disable an approved `browser.enabled` grant. The current implementation starts pinned Playwright MCP 0.0.79 with zero mounts and zero host-published ports. Its fixed entrypoint requires exactly one private IPv4 address and restricts MCP Host validation to that address plus port; DSX independently verifies the same inspected address, injects exactly one per-run harness server named `playwright`, rejects a caller-supplied server with that reserved name, and deletes the browser before Git result capture. A reproducible local recipe exists at `images/browser/Containerfile`; its current local Apple runtime digest is `sha256:dce1d9a9cc9ad38edf545ad29a7f2f3448210a73be3a1cf3651d1c8932b023c0`. Release builds must replace the development-local reference with a compiled published registry digest.
To approve a CLI-enabled browser, inspect and run the identical clone projection; `--browser` changes the executable hash:

```console
$ dsx inspect --mode clone --sandbox web --agent codex --browser
$ dsx run --name web --agent codex --profile work --browser --approve-config <inspected-hash> -- "exercise the application"
```


### Leapp/AWS warning

`aws.mode: "leapp"` is opt-in. `aws.directory` must resolve to a canonical physical, non-symlink directory containing regular non-symlink `config` and `credentials` files. A session-scoped helper copies only a stable paired snapshot into private DSX state, publishes both files as one atomic generation, and mounts that mirror read-only at `/run/dsx/aws`. `AWS_CONFIG_FILE` and `AWS_SHARED_CREDENTIALS_FILE` resolve through `/run/dsx/aws/current`; optional `aws.profile` sets `AWS_PROFILE`. Replacing the approved source directory invalidates the hash. Source rotation that cannot prove a coherent pair fails closed and leaves the last complete generation selected. Abrupt helper death is recoverable only after exact ledger, executable, socket, token, and PID-absence checks. DSX refuses a whole-home/ancestor grant and never renders credential values. **Profile selection is convenience, not credential isolation:** warning `aws_all_profiles_readable` means an agent able to read the mirrored directory may read every profile in it and can exfiltrate those credentials. Release support still requires repeated real Leapp rotation evidence on every supported Apple runtime.

### Destination-specific private relay

Each `network.hostGrants` item validates one name, hostname-or-IP destination, and TCP port. One sandbox-scoped durable helper pins the listener interface, exact owner workspace source IP, resolved destination, and bounded lease. The guest receives only `DSX_BRIDGE_<NAME>_HOST` and `_PORT`; it receives no host identity, Tailscale state, runtime socket, or generic proxy. The relay rejects wildcard/loopback/link-local listeners, public owners, route or address-family mismatch, self-relay, DNS changes, CONNECT/SOCKS/UDP, and alternate sources or destinations. The helper survives the invoking terminal, renews only while fresh Apple inspection corroborates the exact running workspace and complete ownership labels, and stops through authenticated lifecycle control. Failed corroboration allows bounded expiry. Stop/clean terminates the listener and active connections; lifecycle counters contain no payload bytes.

The OAuth callback bridge likewise has no public CLI or configuration surface and is not application-wired. Its internal lease accepts one bounded exact loopback callback with random state and never logs query values, but the pinned harnesses do not expose a validated caller-supplied callback URI plus guest delivery path. Provider login therefore uses only the explicit supported login flow and validated HTTPS host URL opener; DSX does not claim a working guest callback bridge.

### Published ports

Each `ports` entry names a guest TCP port and uses either a fixed host port or `"dynamic"`. Omitting `bind` defaults to `127.0.0.1`; any non-loopback IP value is an explicit, hash-covered trust grant. On Apple `container` 1.2.2, DSX does not trust the accepted-but-reset native publication path: a durable host listener reaches only the exact owned workspace through pinned `container exec` and the read-only guest helper, without exposing the runtime socket. Helper readiness and the atomic manifest are authoritative for fallback bindings; native mappings remain inspect-authoritative on runtime versions that pass the compatibility gate. `list`, `status`, and inspection show final mappings/URLs. Fixed-port conflicts fail without taking over an existing listener. Stop/clean removes exact-owned forwarding.

## 8. Security boundaries

Treat the workspace and every executable declaration as untrusted code:

- The workspace is non-root by default and receives no host home, runtime control socket, SSH/GPG agent, Keychain, Tailscale identity/state, or unrelated repository.
- Setup commands, hooks, dependencies, skills, plugins, MCP servers, and harness configuration have the sandbox authority shown by `inspect`.
- Live mode grants full read/write authority over selected host repositories. Clone mode excludes ignored/untracked host files but does not prevent source exfiltration over allowed internet access.
- Any secret deliberately injected or mounted into a workspace can be read and exfiltrated by the agent. Secret references, not values, belong in configuration.
- The browser boundary isolates untrusted web content from source and credentials; the private network and origin allowlists do not make explicitly proxied workspace data secret.
- DSX uses exact ownership evidence and reverse dependency cleanup. It never adopts or broadly prunes the Apple builder, default network, unrelated Apple resources, host processes, databases, or dependency directories.
- DSX provides Apple microVM isolation, not process-to-process isolation inside the integrated workspace, task scheduling, merge coordination, destination egress policy, Docker Engine APIs, Docker Compose, nested containers, Kubernetes, Rosetta, or amd64 emulation.

Do not follow Docker or Podman setup/cleanup instructions for DSX. Project Dockerfiles may be image build inputs, but DSX's runtime boundary is Apple's `container` CLI.

## 9. Reference-project examples

These examples are review plans, not successful physical-run evidence. They use no credential values or secret host paths.

### `course-intelligence-agency`

The repository has a Dev Container build plus ports. Import only explicitly supported, nonsecret fields; do not import its host AWS bind or host-side initialization hooks.

```jsonc
{
  "$schema": "https://dsx.dev/schema/config-v1.json",
  "schemaVersion": 1,
  "imports": {
    "devcontainer": {
      "path": ".devcontainer/devcontainer.json",
      "fields": ["build", "forwardPorts"]
    }
  },
  "workspace": { "root": "." },
  "image": {
    "build": {
      "context": ".",
      "file": ".devcontainer/Dockerfile"
    }
  },
  "agents": { "default": "codex", "allowed": ["codex", "claude", "omp", "opencode"] },
  "authProfiles": { "work": { "harness": "codex", "persistence": "global" } },
  "browser": { "enabled": false },
  "ports": [
    { "name": "kestra", "guest": 8080, "host": "dynamic", "bind": "127.0.0.1", "protocol": "tcp" },
    { "name": "syllabus", "guest": 3001, "host": "dynamic", "bind": "127.0.0.1", "protocol": "tcp" }
  ]
}
```

```console
$ dsx inspect --root /Volumes/Dev/work/course-intelligence-agency --mode clone --sandbox fix-test --agent codex
$ cd /Volumes/Dev/work/course-intelligence-agency
$ dsx run --name fix-test --agent codex --profile work --approve-config 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef -- "fix the failing test"
$ dsx git diff fix-test
$ dsx git fetch fix-test
$ dsx clean --name fix-test
```

Replace the illustrative approval hash with the exact inspected hash. Add `--browser` only after the browser gate is supported in the target release.

### `devenv`

DSX detects `devenv.nix` facts but does not interpret or execute arbitrary Nix. A maintainer must explicitly translate only reviewed Linux commands, repositories, services, and ports into DSX configuration. This nonsecret skeleton intentionally does not promise that the real composite stack is complete:

```jsonc
{
  "$schema": "https://dsx.dev/schema/config-v1.json",
  "schemaVersion": 1,
  "workspace": {
    "root": ".",
    "members": [
      { "name": "backend", "path": "studocu" },
      { "name": "frontend", "path": "studocu-frontend" }
    ]
  },
  "image": { "ref": "registry.example.invalid/dsx/devenv@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" },
  "agents": { "default": "claude", "allowed": ["claude"] },
  "authProfiles": { "default": { "harness": "claude", "persistence": "global" } },
  "processes": {
    "frontend": {
      "argv": ["yarn", "dev"],
      "cwd": "/workspace/studocu-frontend",
      "required": true,
      "health": { "tcp": { "address": "127.0.0.1:3001" }, "interval": "1s", "timeout": "2s", "retries": 30 }
    }
  },
  "ports": [
    { "name": "frontend", "guest": 3001, "host": "dynamic", "bind": "127.0.0.1", "protocol": "tcp" }
  ]
}
```

`registry.example.invalid` is intentionally non-routable; replace it with a reviewed Linux ARM64 image pinned by digest before approval.

```console
$ dsx inspect --root /Volumes/Dev/work/devenv
$ dsx init --root /Volumes/Dev/work/devenv
$ dsx shell --root /Volumes/Dev/work/devenv --agent claude --profile default --approve-config 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
$ dsx status --root /Volumes/Dev/work/devenv
$ dsx stop --root /Volumes/Dev/work/devenv
$ dsx clean --root /Volumes/Dev/work/devenv
```

Replace the illustrative hash with the exact approved hash.

## 10. Runner quarantine and recovery

Destructive Apple acceptance runs belong only on dedicated physical Apple-silicon runners. A run must attest the host/runtime, acquire the host-local lock, write a unique run ledger before mutation, inventory unrelated sentinels and builder identity, clean only exact proven IDs, and emit evidence JSON. A boot/pre-job sweeper may reconcile a ledger only when ownership and the upstream run's terminal state are certain.

If a ledger, ownership tuple, runtime state, sentinel, builder identity, or run status is uncertain, write `$DSX_CI_STATE_ROOT/QUARANTINED.json`, remove job eligibility out of band, preserve the marker, lock, ledgers, and evidence, and investigate manually. Only a human operator plus an independent reviewer may approve evidence and clear the exact stale lock/marker; any remaining ambiguity keeps the host quarantined. Never use broad `--all`, `prune`, `container system stop`, uninstall, default-network deletion, or builder deletion as recovery. The [runner operations guide](../runner-operations.md) is authoritative. No macOS 26/27 runner, canary, sweeper, quarantine drill, or recovery drill has been provisioned or exercised yet.

## 11. External evidence and release gates

Implemented code and release support are different claims. The following gates are currently external or blocked and must fail closed:

The reproducible dry-run build requires Go 1.26.5, pinned Syft 1.29.0, a concrete SemVer, full 40-character lowercase commit ID, `SOURCE_DATE_EPOCH`, and digest-pinned published `DSX_AGENT_IMAGE` and `DSX_BROWSER_IMAGE` references. It produces `bin/dsx` plus the digest-bound adjacent `bin/dsx-guest`, a deterministic Darwin/arm64 zip, `release-manifest.json`, canonical SPDX 2.3 JSON, and checksums. Those files remain an unsigned dry run.

A release operator must additionally provide a real `Developer ID Application: ...` Keychain identity and a valid `xcrun notarytool` Keychain profile. The gate requires strict code-sign verification with the expected authority and trusted timestamp, notarization status `Accepted` with a submission ID, Gatekeeper (`spctl`) assessment, and a clean temporary-prefix artifact-verification plus read-only `dsx doctor` smoke on a supported physical runner. Missing runtime support fails that smoke; tooling never substitutes an installer or publishes on failure.

| Gate | Current status |
|---|---|
| Registry and image publication identity | Blocked: no production registry identity or published digest-pinned agent/browser image references supplied. |
| Apple signing and notarization identity | Blocked: no Developer ID Application identity or notarytool Keychain profile supplied; unsigned dry-run output is not a release. |
| macOS 26 and 27 physical Apple-silicon runners | Blocked: runners are not provisioned, so destructive, compatibility, browser, relay, Leapp, and end-to-end harness evidence cannot be claimed. |
| Browser private-network/isolation experiment | Observed locally on macOS 27 and Apple `container` 1.2.2: owner pairs connected, both cross-network directions and the host path failed, browser mounts/publications were empty, and exact cleanup removed the experiment. Release support remains blocked on registry/signing and provisioned macOS 26/27 lane evidence. |
| Leapp atomic-rotation experiment | Observed locally on macOS 27 and Apple `container` 1.2.2: repeated complete generations crossed the production mirror's read-only directory mount, guest writes failed, and workspace/network baselines were restored. Release support remains blocked on provisioned macOS 26/27 lane evidence. |
| Private relay experiment | Application wiring exists for live and clone startup. Release support remains blocked pending provisioned-lane private-route, destination-abuse, lease/crash, and cross-sandbox proof. |
| Harness provider authentication, callback, and PTY experiments | Blocked where real provider credentials/flows, callback behavior, OMP closed snapshots, or physical PTY/runtime evidence are absent. The callback lease is internal and deliberately unwired because no pinned harness exposes a validated caller-supplied callback URI plus guest delivery contract. |
| Release | Blocked until the Darwin/arm64 host binary and Linux/arm64 guest helper are reproducibly packaged together; embedded host metadata verifies the adjacent helper digest and pinned image refs; artifact digests, canonical SBOM, signature, notarization, Gatekeeper assessment, clean-install smoke, complete evidence matrix, and security sign-off all pass. |

Hosted macOS CI may compile and run non-virtualized checks only; it is not evidence for Apple nested-virtualization paths. Workflows must use trusted refs and pinned actions. Release tooling must refuse missing identities, credentials, helper/image digests, SBOM, signatures, notarization, or required evidence rather than publishing a partial artifact.
