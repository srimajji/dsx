# DSX (sandbox-dx)

**What:** Sri's personal CLI for isolated Linux dev workspaces (Apple `container` microVMs) seeded from a local Git checkout via verified bundles. Solo tool, not public. Current scale: 1–2 workspaces/project. **Target scale: 5–10 parallel agents on the same codebase (competing implementations workflow).** Target repos: Node/JS monorepos, Laravel monoliths, Terraform/IaC.

## Backlog status (2026-08-14)

Promoted to docs/post-mvp-backlog.md: snapshot commits (POST-MVP-009), ignored-file allowlist (POST-MVP-010), btop-style workspace stats TUI (POST-MVP-011 — improve existing TUI, do not recreate `container ps`).

## Parked ideas

### Snapshot commits for dirty-tree ingress (2026-08-14, revisit later)

Problem: bundles only carry commits; DSX refuses dirty trees, so uncommitted work can't reach the workspace.

Idea: at `dsx create`/`dsx update`, snapshot the dirty working tree — tracked changes + untracked files, ignored still excluded — into a temporary commit on a private ref, without touching the user's branch or index. Mechanics: temp index (`GIT_INDEX_FILE`) + `git add -A` + `git write-tree` + `git commit-tree`, same trick as `git stash create`. Snapshot commit ships in the bundle like any other commit; agent commits on top; results come back as normal commits based on the snapshot.

Keeps: isolation (one-shot copy, no live mount), verified egress/diffability. Trade: point-in-time, not live sync — host edits after create need a re-snapshot.

Related decisions:
- Research doc: docs/research/research-2026-08-13-git-workspace-source-architecture.md (keep bundles; this idea is a middle path the doc's option 5 didn't consider)
- Companion feature: per-project allowlist to inject reviewed ignored files (.env, tfvars) — biggest UX win for Laravel/Node repos
- Scale-out order for the 5–10 agent target (2026-08-14): 1) seed-artifact cache — one bundle per (commit,snapshot), reused for all N; 2) bake dependency layer into project image (or read-only store volume + private overlay); 3) instrument create path, run the doc's 4-concurrent benchmark, split locks only if contention measured; 4) resource budgeting — RAM/disk per microVM likely caps N before git does; 5) `dsx compare` fan-in tooling (range-diff matrix + identical checks); 6) shared seed volume only if reachable sets get big
- Shadow-repo idea for gitless projects: GIT_DIR under ~/.dsx, worktree = project dir; git as transport implementation detail
