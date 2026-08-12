# DSX review: unattended long-running agent hardening

- **Date:** 2026-08-11
- **Scope:** performance, sandbox escape, network configuration, defaults, VM isolation, and multi-day unattended operation
- **Method:** code audit of `internal/`, `cmd/`, `schema/`, `images/` against `docs/PRD.md` and `docs/adr/0001-dsx-implementation-architecture.md`
- **Verdict:** the *safety* posture (ownership proof, fail-closed validation, bounded protocol, argv hygiene) is strong. The *duration* and *egress* posture is not. Nothing below is an Apple-microVM escape; the gaps are in what DSX deliberately hands the guest and in what survives a 24h+ run.

---

## Ranked findings

### 1. `network.internet: false` is inert — there is no egress control at all

- **Where:** `internal/plan/resolve.go:706` builds `BridgeGrant{Kind: "internet"}`; nothing consumes `Kind == "internet"`. `internal/app/host_bridge.go:271` filters on `grant.Kind != "host"`. `internal/app/lifecycle.go:2115` attaches the project network unconditionally. No `--dns`, no `--network none`, no egress policy anywhere in the tree.
- **Why it matters:** the setup wizard offers "Keep offline" (`internal/tui/model.go:286,1003`) and `docs/manual/getting-started.md:76` documents it as a real choice. It changes the plan hash and the inspect output and nothing else. A security toggle that silently does nothing is worse than not offering it.
- **Impact:** an unattended agent — or any transitive dependency it installs — has full DNS + outbound internet in every configuration. This is the primary source-exfiltration path and the main channel for a prompt-injected agent to reach an attacker.
- **Fix:** implement it (attach a network without external routing, or omit the network attachment when `internet == false`), or delete the affordance and document the field as a no-op. Then add a destination allowlist as the real long-run control, since an unattended agent needs registries + provider APIs and little else.

### 2. Guest volumes have no size bound — an agent can fill the host disk

- **Where:** `Adapter.CreateVolume` (`internal/runtime/apple/adapter.go:258-276`) passes `["volume","create", name]` with no size argument. No `size`/`quota` token exists in `internal/runtime/apple/` or `internal/app/`. `docs/PRD.md` never mentions disk.
- **Why it matters:** every clone plans a workspace volume plus one volume per configured volume plus an auth-copy volume (`internal/app/clone.go:676-721`), all host-APFS-backed.
- **Impact:** a looping `pnpm install`, a runaway log, or a `dd` fills the host volume. macOS becomes unusable below a few GiB free, and it takes every other sandbox and the user's machine with it. This directly contradicts "run for long periods without destroying the host".
- **Fix:** add `VolumeSpec.SizeBytes` and pass an explicit size, defaulted per sandbox and configurable next to `resources.cpus`/`resources.memory`. If the installed `container` version cannot bound a volume, add a free-space guard in the clone run loop that stops the sandbox at a floor.

### 3. The agent's life is bound to the foreground `dsx` process; there is no detach/reattach and no wall-clock budget

- **Where:** the harness is a foreground `container exec` owned by `dsx` — `internal/app/clone.go:1758` → `internal/runtime/apple/adapter.go:462-486` → `exec.CommandContext(...).Run()`. SIGHUP cancels the context (`internal/terminal/resize.go:43`). `internal/hostcmd/run.go:16-40` has no `--timeout`/`--max-duration`; `RunClone` gets the raw context from `cmd/dsx/main.go:144`.
- **Why it matters:** an SSH drop, terminal close, or `dsx` crash ends the run. Committed work is recoverable (`prepareCloneResume`, `internal/app/clone.go:365`) but the in-flight turn is lost, and the lease/Leapp helper processes keep running and self-renewing until someone runs `dsx clean`. Conversely a stuck agent holds 4 vCPU / 6 GiB and the project's clone slot forever.
- **Impact:** "unattended for days" only works if the operator wraps `dsx` in `nohup`/`launchd` themselves, which nothing documents.
- **Fix (short term):** add `--max-duration` wrapping the request context, and ignore SIGHUP when stdout is not a TTY. **(Proper):** run the harness as a supervised process in the guest graph — `guestproto.StartParams.Processes` already supports terminal, readiness and wait — so `dsx run` becomes attach/detach over the existing control socket. That is the one architectural change the target use case needs.

### 4. Foreground host bridges hard-expire at exactly 24 hours with no renewal

- **Where:** `internal/app/host_bridge.go:50-58` sets `lease: bridge.MaxTCPLease` (`= 24h`, `internal/bridge/tcp.go:16`) and calls the one-shot path, which does `context.WithTimeout(ctx, grant.Lease)` (`internal/bridge/tcp.go:296-297`). A renewable variant `startLeaseManagedTCP` exists (`internal/bridge/tcp.go:213-222`) and is not used here.
- **Why it matters:** the expiry is wall-clock, so laptop sleep counts against it — an 8h-sleep-per-night Mac hits the wall in ~16h of work.
- **Impact:** at T+24h every approved host-bridge relay closes. The agent's `DSX_BRIDGE_*` endpoints go dead mid-run, silently, with nobody watching. It is the most predictable failure of the target use case.
- **Fix:** switch `activateHostBridges` to `startLeaseManagedTCP` with renewal tied to the run context, or use `context.WithCancel` since the parent run context already bounds the lifetime. Separately, budget leases on monotonic elapsed time so sleep does not consume them.

### 5. Every harness run promotes guest-written credentials into the machine-global auth profile

- **Where:** `internal/app/harness.go:263-274`, `internal/app/clone.go:1760-1767` call `auth.Promote` after *every* run, not just login. `Promote` (`internal/auth/repository.go:287-327`) gates on a baseline-digest concurrency check plus per-harness shape validation only (Claude requires non-empty token strings, `internal/harness/claude/adapter.go:152-207`; Codex requires only valid JSON, `internal/harness/codex/adapter.go:214-244`). The credential file is staged into the guest at `/tmp/dsx-run/<runID>/auth/<artifact>` owned by the agent's uid (`internal/app/harness.go:445-480`), so the agent can overwrite it.
- **Why it matters:** the global profile is not project-scoped (`internal/auth/repository.go:905-910`) and `PrepareGlobalSandbox` seeds every project from it.
- **Impact:** a compromised agent in project A writes attacker-controlled OAuth tokens into `profiles/claude/default`; every later run in every project on that Mac authenticates as the attacker. It survives `dsx clean` by design, so it is also a persistence mechanism, and trivially a cross-project DoS. This is the only finding that crosses a trust boundary *between projects*.
- **Fix:** restrict global-profile promotion to the interactive `dsx login` path; let `run`/`shell` promote only to sandbox-scoped profiles so refresh rotation still works. Additionally bind promotion to credential identity (token subject / `id_token.sub` / `identity_key`) and refuse a silent identity change.

### 6. Repo-resident harness config executes inside the sandbox, outside the approval hash

- **Where:** only OpenCode sets a project-config kill switch (`OPENCODE_DISABLE_PROJECT_CONFIG=1`, `internal/harness/opencode/adapter.go:454-456`) and only OpenCode implements `MCPVerifier` (`:342-393`); `verifyEffectiveMCP` (`internal/app/harness.go:276-305`) is a no-op for Claude, Codex and OMP. Claude/Codex adapters only redirect `HOME`/`CLAUDE_CONFIG_DIR`/`CODEX_HOME` (`claude/adapter.go:306-318`, `codex/adapter.go:197-212`); `--strict-mcp-config` and `-c mcp_servers={}` scope MCP only.
- **Why it matters:** the approval review shows `setup` and `processes` (`internal/tui/model.go:864-870`) but never the repo's `.claude/`, `.codex/`, `.opencode/` surface. A repository's `.claude/settings.json` `hooks` execute on `dsx run` regardless of what the model decides — no approval, no hash change.
- **Impact:** arbitrary code execution inside the VM with the credential file present and internet on. This is the realistic delivery vehicle for finding 5.
- **Fix:** hash and display the repo harness-config surface as part of approval; implement `MCPVerifier` for Claude and Codex; set each harness's project-config-disable switch where one exists.

*(Related, latent: `auth.Repository.Import`/`installReadOnlyConfig` (`internal/auth/repository.go:119-148,555-612`) is dead code today, but as written it would copy the host `settings.json` byte-for-byte into the global profile — including `hooks`. Key-allowlist it before wiring.)*

### 7. Host mounts use a deny-list, and repository-mount containment is enforced only at the app layer

- **Where:** `ValidateHostMountPath` (`internal/config/validation.go:346-385`) denies `$HOME`, `/tmp`, `.ssh`, `.gnupg`, Keychains, browser profiles, `*.sock`, tailscale — and permits everything else. `validMountAuthority` (`internal/runtime/apple/adapter.go:1514-1552`) applies that check to `ConfiguredHost` mounts but gives `Repository` mounts only a lexical `hostPath()` check; containment to the project root lives solely at `internal/app/lifecycle.go:2001-2003`.
- **Why it matters:** `docs/manual/user-guide.md:211` promises the workspace receives "no ... unrelated repository". That clause is not implemented — a repo-shipped `.dsx/config.jsonc` can mount `/Volumes/Dev/work` read-only and expose every sibling repo's `.env` and `.git/config`. Also `/etc`, `/opt`, `/Library`. Separately, repository mounts record no dev/ino identity (host mounts do — `internal/app/authority.go:152-223`), leaving a TOCTOU window between the symlink check at `lifecycle.go:2007` and `container create`.
- **Impact:** approval-gated, but the gate is an opaque hash, so the blast radius is invisible at review time.
- **Fix:** replace the deny-list with an allow-list — source must be inside the project canonical root or inside an explicitly registered shareable root held in DSX state, not a repo-supplied path. Add `RepositoryPlan.SourceIdentity` and re-verify it in the adapter. Render any out-of-project host mount as a distinct high-severity review line.

### 8. Non-loopback publication is self-granting, and the relay then disables peer filtering entirely

- **Where:** `internal/plan/resolve.go:628` sets `ExplicitNonLoopbackGrant: !address.IsLoopback()` — derived from the very field it is meant to gate, so the check at `internal/ports/ports.go:147` can never fire. `internal/runtime/apple/probe.go:593` disables native publication unconditionally, so the hardcoded `127.0.0.1` at `adapter.go:333` is dead code and all publication goes through the relay, where `AllowRemotePeers: !HostIP.IsLoopback()` (`internal/app/host_bridge.go:172`) short-circuits the whole peer check at `internal/bridge/tcp.go:471`.
- **Why it matters:** PRD R10 says "The user must explicitly request non-loopback binding." Under this implementation, writing `"bind": "0.0.0.0"` in a repo-shared config *is* the explicit request; `internal/config/parser_test.go:87-101` asserts zero diagnostics for it.
- **Impact:** an unauthenticated, unfiltered LAN listener forwarding into the microVM, from repo config alone.
- **Fix:** source the grant from a separate `--expose-non-loopback` flag or interactive confirmation, never from the bind address. Reject `IsUnspecified()` listeners in `validatePublicationGrant` and `validateRelaySpecs`. Consider a source-prefix allowlist instead of the all-or-nothing `AllowRemotePeers`.

### 9. Resource admission is per-project only; nothing looks at the host

- **Where:** `validateCloneAdmission` (`internal/app/clone.go:224-262`) counts clones from `ListProjectManifests(ctx, plan.Project.ID)` against `MaxConcurrentClones` (default 1, ceiling 32, `internal/config/validation.go:555`). Correct and TOCTOU-safe under `LockProject`. No code anywhere sums CPU/memory across projects or consults `hw.ncpu`/`hw.memsize`.
- **Why it matters:** a fleet is exactly the multi-project case, and defaults are 4 vCPU / 6 GiB (`internal/app/setup.go:29-30`).
- **Impact:** ten repos = ten projects = 40 vCPU / 60 GiB admitted on a 32 GiB Mac. The machine swaps, the Apple container service starts failing creates, and running sandboxes flip to `StateFailed` on their next inspect.
- **Fix:** add a host-global admission gate before `CreateWorkspace` — enumerate owned workspaces via `runtime.List`, sum planned resources from live manifests, refuse above a configurable fraction of host capacity. Add `resources.hostBudget` to config.

### 10. `container` subprocess amplification puts the PRD R14 budgets at risk

- **Where:** `waitGuestReady` polls every 100 ms for up to 2 minutes (`internal/app/lifecycle.go:34,920-951`), and each poll costs **two** `container` process spawns because `execOnce` re-runs `verifyExpected` → `container inspect` before every `container exec` (`internal/app/guest_client.go:383-390`, `adapter.go:436-465`) — up to 2,400 spawns on a slow start. `LifecycleService.List` (`lifecycle.go:1543-1610`) does an uncached 7-spawn `Probe` then one `container inspect` **per resource per sandbox**, serially, when `List(kind)` would fetch all of them in one call. `cleanManifest` (`lifecycle.go:1460-1520`) does ~7 spawns per resource serially. `Probe()` is called from nine sites with no memoization.
- **Why it matters:** each spawn is a fork/exec plus an XPC round trip to the shared container service — the exact contention that slows every *other* concurrently starting sandbox.
- **Impact:** `dsx inspect` (<500 ms) and planning (<250 ms) are comfortably met — `internal/app/inspect.go` touches no runtime at all. `dsx shell` (<3 s) has ~15-20 spawns of overhead and little headroom. `dsx clean` (<5 s) is at risk for anything but a trivial sandbox (~42 serial invocations plus a manifest fsync after each).
- **Fix:** exponential backoff in `waitGuestReady` (100 ms → 1 s); cache the `verifyExpected` snapshot for the duration of one logical operation; replace the per-record loop in `List` with one `runtime.List` per kind indexed into a map; memoize `Probe()` with a short TTL per CLI invocation.

---

## Also worth fixing (below the top ten)

| Issue | Where | Note |
|---|---|---|
| Guest control socket is authenticated by `SO_PEERCRED` against the **agent's own** uid/gid | `internal/guest/server.go:54-55,261-262` | No privilege gain (children always drop to the child uid with `NO_NEW_PRIVS`), but the agent can `shutdown` the sandbox, signal processes, burn idempotency keys, and fabricate the status/exit/log data the host renders. Add a per-run token passed via the already-allowlisted supervisor argv. |
| Lease helper survives an ungraceful `dsx` exit for up to 24h | `internal/bridge/lease_helper.go:153-176` | A failed ownership check merely stops renewal instead of exiting. Container IPs are pool-allocated, so a later container can inherit the dead workspace's IP and thereby its host-network bridge. Exit after N consecutive failures. |
| Standard-image digest is never verified on the default `make build` path | `internal/app/harness.go:375-399`, `internal/buildinfo/buildinfo.go:10` | `AgentImage` defaults to `"unknown"` and the Makefile does not set it (only `scripts/release/build.sh:44` does), so the running image's bytes are unverified and the browser silently fails to resolve a pinned digest. |
| `env.hostEnv` / `env.secretRef` are accepted and silently resolve to `""` | `internal/config/validation.go:169-184`, `internal/plan/resolve.go:481-487`, `internal/app/guest_client.go:248-251` | Fail-open: the command runs without its credential instead of erroring. Reject at validation until implemented. |
| `appendBounded` copies the full retained buffer on every write once full | `internal/guest/log.go:114-146` | ~1 MiB alloc+memcpy per write under `log.mu` for a chatty agent — days of invisible CPU burn. Use a fixed-capacity ring. |
| Leapp mirror helper polls at 10 Hz, re-reading and SHA-256ing both credential files | `internal/bridge/leapp_mirror.go:27` | ~2.6M cycles per sandbox over three days. Raise the interval or use a kqueue watch. |
| Prompt text is passed on argv | `internal/harness/codex/adapter.go:78`, `claude/adapter.go:98` | Visible in host `ps` and shell history. Pass on stdin. |
| No sleep/wake or network-change handling; host bridge IP resolved once | `internal/app/host_bridge.go:76-83`, `internal/bridge/tcp.go:88-129` | A Wi-Fi→Ethernet switch invalidates the relay bind with no detection or rebind. |
| `docs/PRD.md:328` "run the workspace as a non-root user by default" | `internal/app/lifecycle.go:2081-2098` | PID 1 is root whenever any setup command or process exists (the supervisor, which drops privileges per child). Document the exception. |
| `docs/PRD.md` R11 describes an OAuth callback bridge that is not used | `internal/hostopen/callback.go:68` | `StartCallback` has no production caller; the real flow scrapes the guest terminal for a `claude.com` URL under a strict allowlist. Wire it or correct R11. |

## What held up well

Recording these so they are not accidentally regressed:

- No `--privileged`, `--cap-add`, `--device`, `--security-opt`, or `--rootfs` anywhere. The full `container create` argv is bounded (`internal/runtime/apple/adapter.go:318-343`).
- No host shell, no PATH resolution: absolute validated executable, env of exactly `LANG=C LC_ALL=C`, NUL/CR/LF-checked and length-bounded arguments (`runner.go:47-98`, `probe.go:29`, `adapter.go:1213-1257`). Mount-flag injection is closed (`adapter.go:1554-1580`).
- Guest→host protocol is genuinely bounded: 1 MiB frames, strict JSON with `DisallowUnknownFields`, depth ≤64, duplicate-key rejection, ≤32 connections, per-connection deadlines (`internal/guestproto/protocol.go`, `internal/guest/server.go`).
- Root exec is limited to two byte-exact argv shapes plus a pinned supervisor entrypoint, with the helper independently verifying its own read-only mount from `/proc/self/mountinfo` (`adapter.go:1120-1179,1312-1358`, `internal/guest/trust_linux.go:17-49`).
- Browser isolation is correct and triple-verified: one network, zero mounts, zero ports, re-asserted from inspection (`internal/app/browser.go:68-77,162`, `adapter.go:1052,1077-1102`).
- Secrets never reach argv or host process env — they are written over stdin into a `0600` guest file and passed via `--env-file` (`internal/app/secret_env.go:29-63`).
- State file modes are *verified*, not merely set: `0700` dirs, `0600` files, symlinks rejected, euid checked on every access (`internal/state/fs/manifest.go:467-540`).
- Result bundles are digest-verified, `git bundle verify`'d in a throwaway bare repo, and squash-merged with per-path validation including HFS `.git` aliasing (`internal/gitx/source.go:401-460`, `apply.go:253-283`).
- Credential promotion is exact-file allowlisted — no path smuggles an extra file into a profile (`internal/harness/seed.go:19-48`).
