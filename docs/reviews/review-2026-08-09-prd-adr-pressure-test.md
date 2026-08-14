# Review: PRD + ADR 0001 pressure test

- **Date:** 2026-08-09
- **Scope:** [PRD.md](../PRD.md), [ADR 0001](../adr/0001-dsx-implementation-architecture.md)
- **Status:** Feedback only — no changes made to the originals
- **Reviewer note:** Findings ordered by severity. Item 6 was revised after discussion (see inline).

## Critical

### 1. The macOS floor is load-bearing and unstated

PRD R1 says "supported macOS versions" without pinning one. Apple `container` on macOS 15 has no `container network` command, isolates containers from each other on vmnet, and container IPs aren't reachable from the host. Apple explicitly won't fix macOS-15-only issues. The browser↔workspace private project network (PRD R11, ADR §4) and possibly loopback port publishing don't exist on macOS 15.

**Suggestion:** Pin macOS 26+ as a hard requirement in R1 and `dsx doctor`, or define a degraded mode without the browser VM.

Sources: [apple/container technical overview](https://github.com/apple/container/blob/main/docs/technical-overview.md), [4sysops: Apple Container CLI on macOS 15 vs 26](https://4sysops.com/archives/install-apple-container-cli-running-containers-natively-on-macos-15-sequoia-and-macos-26-tahoe/)

### 2. Host↔guest control channel is undecided

`dsx-guest` must "report readiness and exit status to the host" (ADR §5), but the transport is never chosen: vsock, `container exec` polling, a published loopback control port, or file-based signaling. This affects health-check latency, log streaming, cancellation, and crash detection.

**Suggestion:** Decide before slice 2; deserves its own ADR section or ADR 0002.

### 3. `dsx apply` semantics are underspecified

The diff/apply workflow never defines:

- Conflict strategy when the host repo advanced since the clone was taken (3-way merge, patch-reject, refuse?).
- Whether apply transfers commits or a working-tree diff.
- Handling of new untracked/binary files created by the agent.
- Partial apply across composite-workspace repos.

This is where users lose work — it needs requirements-level definition in the PRD.

### 4. Private-clone mechanics contradict the worktree rejection

ADR rejects worktrees for sharing the host Git object database, but a local `git clone` hardlinks objects by default — same shared inodes. Also unspecified: does the clone live on host disk (mounted in) or inside a guest volume?

**Suggestion:** If host-side, require `--no-hardlinks` (or `--dissociate`); state the clone location explicitly.

### 5. `dsx-guest` injection into project images

ADR §5 embeds the helper in the *standard* image, but R4 allows project images/builds. Specify the mechanism for custom images — runtime bind-mount of the static binary is the usual approach, and a point in favor of the Go static-binary choice worth stating.

## Significant gaps

### 6. Per-process log routing convention (revised)

~~Original finding proposed `dsx logs` / `dsx ps` / `dsx exec` commands.~~ Revised: Apple's CLI already provides `container logs` and `container exec`; duplicating them in DSX is scope creep. The residual gap is guest-side: `container logs` only shows container stdout/stderr — in the integrated topology that's whatever `dsx-guest` (PID 1) emits, not per-process streams. R5 promises per-process "log identity," so the ADR should state how `dsx-guest` routes it:

- Multiplex all process output to container stdout with a process-name prefix (so `container logs` + grep covers it), and/or
- Write per-process files to a known path (e.g., `/var/log/dsx/<process>.log`) reachable via `container exec`.

Behavior should be consistent between dsx-guest-managed and `process-compose`-managed projects. `container ls` plus DSX's name/label scheme is arguably enough for project-resource discovery in the MVP.

### 7. Builder VM ownership

`container build` runs BuildKit in a shared builder container — a cross-project global resource. Cleanup's "delete only what DSX owns" rule must explicitly classify it. First-build cost (builder boot) belongs in R14's startup timing breakdown.

### 8. "No mandatory daemon" needs scoping

Apple `container` itself runs launchd services (`container system start` / apiserver). The claim is true for DSX-added components; scope the wording and have `doctor` verify/start the Apple service.

### 9. Zero quantified targets

"Fast" appears throughout with no numbers. Even loose budgets (warm `dsx shell` ≤ Xs, idle memory ≤ Y GB, `inspect` ≤ 500ms) would make the ADR's revisit criteria ("review measured startup time") testable.

### 10. Virtiofs live-mount performance and file-watching

Known pain point for JS monorepos: slow I/O on mounted `node_modules` (mitigated by the dep-volume design — good) and inotify events often not propagating across the boundary, which breaks Vite/webpack watch mode.

**Suggestion:** Add an explicit slice-1 test: HMR works against a live mount.

### 11. Port-conflict detection is TOCTOU

"Detect host port conflicts before launch" (R10) races between check and bind. Prefer bind-first-then-hand-off, or treat publish failure as the authoritative signal.

### 12. Unattended config approval

`--force` bypasses destructive-command confirmation, but the R12 approval-hash flow in non-interactive contexts (CI, scripts) is undefined — fail closed, env var, or pre-approved hash file?

### 13. CI strategy for the adapter

ADR commits to compatibility tests against supported `container` releases, but Virtualization.framework workloads generally can't run on standard GitHub-hosted macOS runners (nested virtualization limits). Plan for self-hosted Apple silicon runners and state it.

### 14. Who drives the browser?

R11/slice 4 give the workspace a Playwright endpoint, but the agent needs a client to use it (Playwright MCP or harness-native browser tools pointed at the browser VM over the project network). Specify how each harness gets configured to reach it.

### 15. Concurrent commands on one project

Cross-project isolation is well covered (PRD §5.3); same-project concurrency isn't — `dsx shell` while `dsx run` is active? Two `run`s? Define allowed/refused/queued.

## Minor

- PRD §2 lists PHP as supported; acceptance criterion #2 lists only Node, Python, Java (Laravel only appears via criterion #7). Align them.
- ADR cites `container` 1.2.2; `doctor` should encode a tested version *range*, and the ADR should state the compatibility policy (e.g., track 1.x, gate on integration suite).
- x86-only dependencies: ARM64-only is stated, but Containerization supports Rosetta for amd64 binaries — explicitly declare it in or out of MVP scope.
- Auth volumes hold plaintext OAuth tokens in VM disk images on host disk. Acceptable residual risk, but list it in PRD §8 alongside the others.
- Restart behavior (R5) has no semantics — backoff, max retries, "failed" state. Fine to defer, but say so.

## Open questions

1. Can macOS 26+ be required outright, or do target users still run Sequoia? Decides finding #1 and simplifies much if yes.
2. For `dsx run`, is apply expected to preserve agent commit history, or is a squashed working-tree diff acceptable? Shapes finding #3.
3. Does the `devenv` workspace have any amd64-only dependencies (Rosetta scope question)?
4. The ADR revisit date (2026-08-23) gives two weeks for both reference workspaces through slice 2+ — intentional forcing function, or should the revisit trigger be slice-based only?

## Sources

- [apple/container technical overview](https://github.com/apple/container/blob/main/docs/technical-overview.md)
- [4sysops: Apple Container CLI on macOS 15 (Sequoia) and macOS 26 (Tahoe)](https://4sysops.com/archives/install-apple-container-cli-running-containers-natively-on-macos-15-sequoia-and-macos-26-tahoe/)
- [apple/container releases](https://github.com/apple/container/releases)
