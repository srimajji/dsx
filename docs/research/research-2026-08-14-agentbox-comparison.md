# AgentBox comparison research

- **Date:** 2026-08-14
- **Status:** Research note
- **Scope:** Comparison of [madarco/agentbox](https://github.com/madarco/agentbox) against DSX's source-transfer, trust, and parallel-agent architecture
- **Related:** [Git workspace source architecture research](./research-2026-08-13-git-workspace-source-architecture.md), [PRD](../PRD.md), [ADR 0001](../adr/0001-dsx-implementation-architecture.md), [post-MVP backlog](../post-mvp-backlog.md)
- **Pinned revision:** [`1de12d36feac92ed5108a8c9fb3606d47d13d3ac`](https://github.com/madarco/agentbox/tree/1de12d36feac92ed5108a8c9fb3606d47d13d3ac)

## What AgentBox is

A TypeScript CLI that runs multiple coding agents (Claude Code, Codex, OpenCode) in parallel "boxes." The default backend is a local Docker/OrbStack container per box; cloud backends exist for Hetzner and Daytona. Each box gets a tmux-attached agent session, an in-box VS Code/Cursor server, a noVNC browser, an in-box `agentbox-ctl` supervisor, and a host relay process for privileged operations. It targets the same workflow DSX is heading toward: 5–10 parallel agents against one codebase on one machine.

This comparison extends the [2026-08-13 landscape table](./research-2026-08-13-git-workspace-source-architecture.md#broader-sandbox-landscape) with a system not covered there.

## Architecture summary

| Concern | AgentBox (local Docker path) | DSX |
|---|---|---|
| Isolation unit | Docker/OrbStack container, shared Linux kernel | Apple `container` microVM per workspace |
| Source ingress (git) | `git worktree add` executed inside the container against the host repo's `.git/`, bind-mounted read-write at its identical absolute path | Verified mode-0600 Git bundle into a guest-owned private repository |
| Uncommitted state | Host `git stash create` SHA replayed in-box, plus tar-piped untracked files (gitignore-respecting) | Not supported yet; identical mechanism proposed as POST-MVP-009 |
| No-git projects | `tar` pipe from the host workspace, optionally from an APFS `cp -c` clone (`--host-snapshot`) | Not supported; shadow-repo idea parked, no backlog entry yet |
| Cloud ingress | Clone-and-tar with depth heuristics; `git bundle` was evaluated and rejected for shallow seeding | Full reachable bundle, no shallow |
| Result egress | `agentbox download` (gitignore-aware rsync back), or relay-mediated push: `git push` runs on the host with host credentials | Committed result bundle → `refs/remotes/dsx/<ws>` → guarded apply |
| Credentials | Never in the box for git push (host relay); shared `~/.claude` volume mounted into every box for harness identity | Isolated per-workspace copies from a canonical DSX store |
| Warm start | Checkpoints: `docker commit` of a warm box (deps installed, caches hot), restored as the base image of new boxes | None; dependency install repeats per workspace |
| Idle strategy | `docker pause` (cgroup freezer): 0 CPU, sub-second resume | `container stop`/start; no pause equivalent in Apple 1.2.2 |
| Fleet visibility | `agentbox dashboard`, `agentbox top`, per-box status daemon | TUI exists; stats view is POST-MVP-011 |

Implementation evidence (pinned): [`docs/architecture.md`](https://github.com/madarco/agentbox/blob/1de12d36feac92ed5108a8c9fb3606d47d13d3ac/docs/architecture.md), [`packages/core/src/sync/workspace.ts`](https://github.com/madarco/agentbox/blob/1de12d36feac92ed5108a8c9fb3606d47d13d3ac/packages/core/src/sync/workspace.ts) (stash-create + untracked replay), [`packages/sandbox-docker/src/sync/in-box-git.ts`](https://github.com/madarco/agentbox/blob/1de12d36feac92ed5108a8c9fb3606d47d13d3ac/packages/sandbox-docker/src/sync/in-box-git.ts) (in-container worktree), [`packages/sandbox-cloud/src/sync/workspace-seed.ts`](https://github.com/madarco/agentbox/blob/1de12d36feac92ed5108a8c9fb3606d47d13d3ac/packages/sandbox-cloud/src/sync/workspace-seed.ts) (bundle rejection rationale for shallow cloud seeding).

## Where AgentBox independently validates DSX decisions

1. **Snapshot-commit ingress works in production.** AgentBox ships exactly the POST-MVP-009 mechanism: `git stash create` for tracked changes plus a tar pipe of `ls-files --others --exclude-standard` output for untracked files, replayed in the box without touching the user's branch or index. This removes most novelty risk from POST-MVP-009.
2. **Shallow bundles are a dead end.** Their cloud seeding code comments that `git bundle create` has no shallow support and that portable workarounds "all produce bundles with unsatisfiable prerequisites" — corroborating the 2026-08-13 research's caution against composing shallow repositories with bundles.
3. **Host node_modules are never reusable.** Rejected for macOS-binary/Linux-runtime mismatch; the same constraint applies to DSX and confirms dependency warming must happen guest-side.
4. **Credential forwarding into the sandbox is rejected there too.** Their push path runs on the host with host credentials over an RPC relay; the box never holds git credentials. Same philosophy as DSX's guarded apply and isolated auth stores.
5. **Their own retired design mirrors the 2026-08-13 conclusion.** A FUSE overlay with the host tree as lowerdir was retired: "the worktree *was* the isolation, the overlay added nothing." Live filesystem overlay of host source lost on its merits even in a lower-trust product.

## Where AgentBox sits below DSX's trust floor

- **Read-write bind mount of the host `.git/` into every box.** An agent can write `.git/hooks/*` or `.git/config` (`core.fsmonitor`, hook paths); arbitrary code then executes on the host the next time the user runs any git command. The credential relay does not close this vector. This is precisely the "no shared writable Git metadata" invariant DSX's bundle transport exists to enforce; do not import this ingress model.
- **Shared object database and refs space across all N boxes.** Host `git gc`, ref races, and index/lock contention scale with box count. DSX's per-workspace private repository avoids the entire class.
- **Shared writable `~/.claude` volume across boxes.** Convenient for identity, but a compromised box can poison skills/config consumed by every other box — the cross-contamination DSX's per-workspace credential copies prevent, and a direct conflict with the competing-implementations requirement that competitors share nothing writable.
- **Container, not VM, isolation on the local path.** Shared-kernel isolation is a weaker boundary than Apple microVMs; orthogonal to source transport but part of the overall floor difference.

## Takeaways for DSX

| Takeaway | Disposition |
|---|---|
| Warm-start checkpoints (`docker commit` of a box with dependencies installed and caches hot; new boxes start from that image in ~1s) | The strongest idea in the repo for DSX's 5–10 agent target. Apple `container` 1.2.2 exposes no `commit` or volume-snapshot surface, so the DSX-shaped version is: bake the dependency layer into the project image and rebuild on lockfile change. Revisit full workspace checkpointing when Apple ships image-commit or volume-snapshot APIs (existing watch item in the 2026-08-13 research). |
| Relay-mediated host-side push | Candidate for a future `dsx push`: host pushes an explicitly approved `refs/remotes/dsx/<ws>` ref upstream using host credentials; the workspace never holds them. Complements guarded apply in the egress direction. Requires PRD addition; fits the existing typed-grant philosophy (compare POST-MVP-007). |
| Fleet visibility (`dashboard`, `top`, per-box status daemon) | Confirms POST-MVP-011 scope. Their open-questions list independently names a "cross-box diff dashboard showing `git diff --stat` per box" — the `dsx compare` concept. |
| Idle-resource policy (`docker pause`, auto-pause on inactivity) | No Apple pause equivalent; DSX's version is auto-stop of idle workspaces (state survives on the named volume) with slower resume. Worth a small backlog entry when parallel use begins. |
| Uncommitted-state replay mechanism | Direct prior art for POST-MVP-009; reference their `workspace.ts` when writing the ADR delta. |

## Decision ledger

- **Decision:** No change to DSX transport or trust architecture. AgentBox strengthens the case for the current model: its convergent choices validate DSX's direction, and its divergent choices (writable `.git` mount, shared harness volume) are exactly the vectors DSX's invariants exclude.
- **Actions:**
  1. Reference AgentBox's replay implementation in the POST-MVP-009 ADR delta.
  2. Add dependency-layer image baking to the backlog as the Apple-compatible checkpoint substitute.
  3. Consider `dsx push` (relay-style, host-side) as a future backlog candidate.
  4. Keep auto-stop-on-idle in mind for the parallel-agent phase.
- **Revisit triggers:** Apple `container` gains image commit or volume snapshot (enables true checkpoints); DSX begins regular 5–10 agent runs (idle policy and fleet dashboard become active work).
