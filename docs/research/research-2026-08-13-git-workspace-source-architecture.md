# Git workspace source architecture research

- **Date:** 2026-08-13
- **Status:** Research recommendation
- **Scope:** Source ingress, private workspace storage, parallel-agent isolation, and committed result egress
- **Related:** [Product requirements](../PRD.md), [implementation architecture](../adr/0001-dsx-implementation-architecture.md)

## What and why this was researched

DSX currently creates each workspace as an Apple `container` microVM with a guest-owned private Git repository. It transfers committed source and results using restrictive, verified Git bundles instead of mounting the host checkout into the guest.

This research evaluated whether that architecture should change, particularly for repositories with large host Git object stores and for several long-running agents working on different features or competing implementations from the same base commit.

The questions were:

1. Does DSX copy an entire large host object database into every workspace, or only objects reachable from the selected source revision?
2. Would live filesystem synchronization, raw file copy, shallow clones, or Apple named-volume seeding make workspace creation materially faster without weakening DSX's security boundary?
3. How do Docker Sandboxes, Microsandbox, Daytona, E2B, OpenHands, and GitHub Codespaces move source into isolated environments and return results?
4. Which non-Git source-transfer mechanisms are supported by Apple `container` 1.2.2?
5. What architecture is appropriate for `/Volumes/Dev/work/course-intelligence-agency` when multiple agents work concurrently?

The research preserved DSX's existing security floor:

- No agent-visible host source mount.
- No shared writable Git metadata or object database.
- No host home mount.
- No ignored or untracked host files crossing implicitly.
- One private writable checkout per workspace.
- Explicit, verified result retrieval before host integration.

## Decision

Keep DSX's current model:

- One guest-owned private Git repository per workspace.
- One workspace-unique Apple named volume for persistent source and Git state.
- Verified Git bundles as the default source and result transport.

Do not replace Git transfer with live filesystem synchronization for the current Git-workspace product.

Apple volumes and Git bundles solve different problems:

| Concern | Mechanism |
|---|---|
| Private persistent workspace storage | Apple named ext4 volume |
| Exact source commit and ref identity | Git |
| Compressed reachable-object transfer | Git pack/bundle |
| Offline and local-only commits | Git bundle |
| Update ancestry and conflict behavior | Git rebase and refs |
| Independent index, refs, and object database | Private guest repository |
| Reviewed result fan-in | Namespaced host ref and guarded apply |

Replacing bundle transport with raw files would require DSX to own inclusion rules, deletion semantics, content hashes, generations, atomic staging, conflict detection, crash recovery, and reviewed result integration. Git already provides those semantics.

This remains a two-way-door optimization decision. Measure first. If transfer is material, optimize within the Git protocol before introducing a filesystem reconciliation subsystem.

## Current DSX architecture

```mermaid
flowchart LR
    A[Clean symbolic host branch] --> B[Private ref<br/>refs/dsx/private/source/nonce]
    B --> C[Mode-0600 bundle<br/>maximum 512 MiB]
    C --> D[SHA-256, Git, and exact-ref verification]
    D --> E[Apple container copy]
    E --> F[Guest git init and fetch]
    F --> G[Private branch<br/>dsx/workspace]

    G --> H[Committed result bundle]
    H --> I[refs/remotes/dsx/workspace]
    I --> J[Explicit guarded apply]
```

The concrete flow is:

```text
clean symbolic host branch
  -> refs/dsx/private/source/<random nonce>
  -> mode-0600 bundle, hard limit 512 MiB
  -> SHA-256 + Git validity + exact advertised ref/commit verification
  -> Apple `container copy`
  -> guest `git init`
           `git fetch <bundle> <private-ref>`
           `git checkout -B dsx/<workspace> <source-commit>`

workspace commit
  -> verified mode-0600 result bundle
  -> refs/remotes/dsx/<workspace>
  -> explicit guarded `dsx git apply`
```

Implementation evidence:

- [`internal/gitx/source.go`](../../internal/gitx/source.go) performs clean-state checks, repository identity revalidation, private-ref creation, bounded bundle production, hashing, bundle verification, and exact advertised-ref verification.
- [`internal/gitx/contracts.go`](../../internal/gitx/contracts.go) defines mode `0600`, `refs/remotes/dsx/`, and the 512 MiB source/result limits.
- [`internal/app/workspace.go`](../../internal/app/workspace.go) prepares sources, persists manifest intent, creates private volumes, copies bundles, and initializes/fetches/checks out the guest repository.
- [`internal/app/workspace_git.go`](../../internal/app/workspace_git.go) handles update/rebase, result export, namespaced fetch, transactional apply, and removal protection.
- The normative product and architecture contracts are the [PRD](../PRD.md) and [ADR 0001](../adr/0001-dsx-implementation-architecture.md).

### Strengths

- Works offline and with commits that have never been published to a remote.
- Transfers committed reachable objects only.
- Untracked and ignored host files, including `.env`, do not cross.
- Host source and home directories are not left mounted.
- Every workspace owns its working tree, index, refs, and object database.
- Result fetch updates a dedicated host ref rather than mutating the host working tree.
- Workspace removal detects uncommitted files, rebase state, unfetched commits, and altered fetched refs.

### Current limits

- Host tracked state must be clean.
- Untracked and ignored inputs are excluded.
- Guest work must be committed before normal bundle egress.
- Create, update, and result bundles currently include all objects reachable from the advertised ref rather than a negotiated delta.
- Source and result artifacts fail above 512 MiB.
- DSX does not currently define reviewed submodule recursion or Git LFS materialization behavior for workspace source transfer.
- Same-project lifecycle and Git operations take the workspace lock followed by the project lock. This serializes portions of the host control plane even when separate workspace VMs run concurrently.

## Measured repository: `course-intelligence-agency`

Read-only measurements were taken on 2026-08-13 after verifying that tracked state was clean.

| Observation | Value |
|---|---:|
| Branch | `feat/core-14-improve-insource-flow-performance` |
| Commit | `8ccea4db83c4a4832613ca3352a8ac2b18c94ea9` |
| Tracked files | 141 |
| `.git/objects/pack` | 3,438,120 KiB, approximately 3.28 GiB |
| `git -c maintenance.auto=false bundle create - HEAD` | 1,253,974 bytes; 0.08 seconds |
| `git -c maintenance.auto=false archive --format=tar HEAD` | 2,621,440 bytes; 0.02 seconds |

The 3.28 GiB host object store is not the per-workspace source cost. It may contain objects reachable from other refs and historical repository activity. The clean-HEAD bundle was approximately 1.20 MiB and was already smaller than the approximately 2.50 MiB tracked-tree tar.

`git bundle create - HEAD` is a useful proxy for objects reachable from the selected clean commit. It is not the literal DSX artifact: DSX advertises a random private ref and adds repository-identity, digest, size, and exact-ref verification.

The measurement does not justify changing source transport for this repository.

## Docker Sandboxes comparison

Docker supports two materially different workspace modes.

| Mode | Ingress and visibility | Parallel and result behavior | DSX consequence |
|---|---|---|---|
| Direct | Host tree is a read-write filesystem passthrough. Changes are instant because there is no sync process. | Agents sharing the same tree can race over files, index, branch, hooks, and generated output. | Convenient, but below DSX's source-isolation floor. |
| `--clone` | The complete host Git root is mounted read-only at `/run/sandbox/source`; a separate writable clone is created in the VM. | An in-VM Git daemon exposes `sandbox-<name>` to the host for explicit fetch. Removing before fetch or push loses private work. | Closest comparison, but the read-only mount still exposes untracked and ignored host files. |

Docker explicitly documents that clone mode protects the host repository from modification, not inspection. Ignored files such as `.env` remain readable by the agent. Clone mode also rejects linked host worktrees. Direct mode is not an isolation model for independent writers against one host tree.

Sources:

- [Docker Sandboxes architecture](https://docs.docker.com/ai/sandboxes/architecture/)
- [Docker Sandboxes isolation](https://docs.docker.com/ai/sandboxes/security/isolation/)
- [Docker Sandboxes usage](https://docs.docker.com/ai/sandboxes/usage/)
- [Docker Sandboxes workflows](https://docs.docker.com/ai/sandboxes/workflows/)

The relevant lesson is the private writable clone and explicit Git fan-in. DSX should not import Docker's host-tree exposure.

## Microsandbox

At pinned commit [`c3d513cb18d0cddc59294f944a27a10688033d25`](https://github.com/superradcompany/microsandbox/tree/c3d513cb18d0cddc59294f944a27a10688033d25), Microsandbox provides general storage and transfer primitives:

- Bind mounts and persistent named volumes: [volumes](https://github.com/superradcompany/microsandbox/blob/c3d513cb18d0cddc59294f944a27a10688033d25/docs/sandboxes/volumes.mdx).
- Filesystem read/write and host-to-guest copy: [filesystem](https://github.com/superradcompany/microsandbox/blob/c3d513cb18d0cddc59294f944a27a10688033d25/docs/sandboxes/filesystem.mdx).
- Recursive host/sandbox copy: [`copy.rs`](https://github.com/superradcompany/microsandbox/blob/c3d513cb18d0cddc59294f944a27a10688033d25/crates/cli/lib/commands/copy.rs).
- SSH, SFTP, scp, and rsync compatibility: [SSH](https://github.com/superradcompany/microsandbox/blob/c3d513cb18d0cddc59294f944a27a10688033d25/docs/sandboxes/ssh.mdx).
- Stopped-sandbox writable-layer snapshots: [snapshots](https://github.com/superradcompany/microsandbox/blob/c3d513cb18d0cddc59294f944a27a10688033d25/docs/sandboxes/snapshots.mdx).

Microsandbox does not define a Git or `gh` synchronization architecture. Repository clone, commit, push, pull request creation, and conflict handling remain user or agent code. Its snapshots can amortize environment setup, but do not provide reviewed repository result integration.

## Broader sandbox landscape

| System | Ingress | Persistence | Egress | Credential placement | Same-repository parallelism | DSX lesson |
|---|---|---|---|---|---|---|
| Docker Sandboxes | Read-write bind, or read-only host Git root followed by private clone | Persistent VM | Instant files, or sandbox Git remote | Host proxy and agent configuration | Direct mode races; clone mode still needs branches/worktrees | Private clone is sound; host-tree exposure is weaker |
| Daytona | Git clone into sandbox or filesystem API | Sandbox filesystem; optional shared volumes | Commit/push or file download | PAT per operation; optional proxied secrets | Private sandbox checkout per worker | Git remains the repository fan-in protocol |
| E2B | Full or shallow Git clone, or filesystem API | Pause/resume; beta volumes | Commit/push or file download | Inline credentials or readable on-disk helper | Separate sandbox and branch per worker | Snapshots reduce setup cost, not result-integration cost |
| OpenHands | Cloud repository/branch selection; local Docker can mount a host tree read-write | Conversation and sandbox state | Frequent commit/push; Cloud can open pull requests | Short-lived GitHub token; local environment forwarding | Separate cloud work; local shared mounts retain race risk | Prefer remote Git fan-in over shared local writes |
| GitHub Codespaces | Shallow GitHub clone into `/workspaces` | Per-codespace storage across stops and rebuilds | Commit/push/pull request | Expiring repository-scoped token; secrets in environment | One isolated VM and checkout per codespace | A canonical remote simplifies identity but requires publication and network access |

Primary sources:

- Daytona: [Git operations](https://www.daytona.io/docs/en/git-operations/), [volumes](https://www.daytona.io/docs/en/volumes/), and [secrets](https://www.daytona.io/docs/en/secrets/).
- E2B: [Git integration](https://e2b.dev/docs/sandbox/git-integration), [persistence](https://e2b.dev/docs/sandbox/persistence), and [volumes](https://e2b.dev/docs/volumes).
- OpenHands: [first projects](https://docs.openhands.dev/overview/first-projects), [GitHub integration](https://docs.openhands.dev/openhands/usage/cloud/github-installation), and [local Docker sandbox](https://docs.openhands.dev/openhands/usage/sandboxes/docker).
- GitHub Codespaces: [deep dive](https://docs.github.com/en/codespaces/about-codespaces/deep-dive), [lifecycle](https://docs.github.com/en/codespaces/about-codespaces/understanding-the-codespace-lifecycle), and [security](https://docs.github.com/en/codespaces/reference/security-in-github-codespaces).

The recurring safe pattern is one private writable checkout per agent plus explicit Git fan-in. Volumes, templates, snapshots, and prebuilds reduce environment setup cost; they do not replace result integration.

## Apple `container` 1.2.2 feasibility

DSX currently accepts Apple CLI/server versions `>=1.2.2 <1.3.0` and identifies the tested pair as `apple-container/cli-1.2.2/server-1.2.2` in [`internal/runtime/apple/probe.go`](../../internal/runtime/apple/probe.go).

| Mechanism | Apple 1.2.2 status | Assessment |
|---|---|---|
| Host bind mount, read-write or read-only | Supported by `--volume` and `--mount`; the pinned command reference supports `readonly`. Rolling Apple docs identify `bind` and `virtiofs` as aliases. | Instant shared view and no sync, but exposes the host path. Reject for DSX's default trust model. |
| Private named ext4 volume | Supported and already used by DSX. | Correct persistent workspace storage. Does not itself define ingress or egress. |
| `container copy` | Supported between the host and a running container for files and directories. | Useful one-shot transport. No documented delta, resume, atomic publish, generation, or conflict semantics. |
| Deterministic tracked-tree archive | Implementable over `container copy` or `exec -i`. | Isolated and simple, but loses refs, ancestry, history, and normal result integration unless Git metadata is retained separately. |
| rsync or content-addressed manifest | Implementable through `dsx-guest`. | DSX must own inclusion, deletion, hashing, staging, generations, conflicts, and crash recovery. High permanent complexity. |
| Remote `git` or `gh` clone | Implementable inside the guest. | Can use shallow or partial transfer, but requires a published exact commit, network access, and provider credentials. Changes DSX's local-only and offline contract. |
| OCI source image | Supported through normal image operations. | Adds image-store churn and source retention, and still needs a separate result protocol. |
| Named-volume snapshot, clone, or import | Not present in Apple 1.2.2's supported volume command surface. | Future watch item. Do not manipulate runtime-private `volume.img` files directly. |

Primary Apple sources:

- [Apple `container` 1.2.2 command reference](https://github.com/apple/container/blob/1.2.2/docs/command-reference.md)
- [`ContainerCopy.swift`](https://github.com/apple/container/blob/1.2.2/Sources/ContainerCommands/Container/ContainerCopy.swift)
- [`VolumeCreate.swift`](https://github.com/apple/container/blob/1.2.2/Sources/ContainerCommands/Volume/VolumeCreate.swift)
- Rolling [mount and volume documentation](https://github.com/apple/container/blob/main/docs/volumes.md), used only where the 1.2.2 documentation does not name the underlying mount alias
- DSX [`CreateVolume`, `CreateWorkspace`, `CopyTo`, and `CopyFrom`](../../internal/runtime/apple/adapter.go)

## Option scorecard

`Strong`, `Yes`, and `Low` are favorable except in the final complexity column.

| Option | Isolation | Exact Git identity | Offline/local commits | Uncommitted fidelity | Initial fan-out | Incremental cost | Network credentials | DSX complexity |
|---|---|---|---|---|---|---|---|---|
| 1. Current full bundle and private repository | Strong | Yes | Yes | No | Reachable history | Full reachable ref | None | Low; implemented |
| 2. Full initial bundle, then incremental bundles | Strong | Yes | Yes | No | Same as current | Potentially low | None | Medium; experiment required |
| 3. Transient read-only Git seeder and private repository | Host source readable during seed | Potentially, after testing | Yes | No | Potentially low with filtering or shallow history | Potentially low | None | High |
| 4. Remote shallow/partial clone and push/pull request | Strong | Published commits only | No | No | Low | Low | Required | Medium; different product mode |
| 5. Tracked archive or DSX manifest/delta protocol | Strong | No, unless Git is retained separately | Yes | Policy-dependent | Tree-sized | Low | None | Very high |
| 6. Live host bind or shared worktree | Weak | Shared and racy | Yes | Yes | Near zero | Instant | None | Low code, high security and recovery cost |
| 7. Future Apple volume snapshot/clone | Potentially strong | Only if seeded with Git | Yes | Snapshot-dependent | Potentially low | Unknown | None | Unsupported and unknown |

### Option decisions

1. Keep option 1 as the default.
2. Prototype option 2 only if measurement shows bundle production or copy is material.
3. Treat option 3 as fallback research for genuinely large reachable histories.
4. Offer option 4 only as an explicitly different GitHub-authoritative product mode.
5. Build option 5 only if DSX intentionally adds non-Git or uncommitted-source workflows.
6. Reject option 6 for the current security tier.
7. Watch option 7 during Apple compatibility upgrades.

Git supports full and incremental bundles, but bundle verification requires prerequisite commits to exist and be fully linked in the receiving repository. The bundle format does not encode shallow-repository boundaries. Do not claim that shallow repositories and incremental bundles compose safely until a focused update and result-egress experiment proves it. See the [Git bundle documentation](https://git-scm.com/docs/git-bundle).

## Recommendation for `course-intelligence-agency`

### Agents implementing different features

Give every agent:

- The complete 141-file tree.
- Its own workspace and `dsx/<workspace>` branch.
- Private dependency, cache, service, process, and authentication state.
- Dynamic ports where needed.

Sparse checkout has little value here. The reachable bundle is only approximately 1.20 MiB, while root lockfiles, workspace configuration, shared packages, and Turbo scripts span the monorepo. Orchestrator synchronization also copies app-owned flows into committed generated paths under `apps/orchestrator/flows/processes/`, crossing app boundaries.

### Agents producing competing implementations

1. Seed every workspace from exactly `8ccea4db83c4a4832613ca3352a8ac2b18c94ea9`.
2. Grant equivalent, explicitly approved credentials and fixtures.
3. Keep ignored writable state separate. The repository currently ignores `.env`, `.devenv`, and `.direnv`; none should be shared between competitors.
4. Require each result to be committed.
5. Fetch into separate `refs/remotes/dsx/<workspace>` refs.
6. Compare implementations with normal Git `diff` or `range-diff` and identical behavioral checks.
7. Never share a writable source tree, generated-output directory, dependency cache, or service-data volume between competing implementations.

Changing source transport is not justified by this repository's current size. The larger likely concurrency costs are:

- Project-lock wait in the DSX host control plane.
- Repeated dependency installation and build caches.
- Duplicated databases and project service state.

Filesystem synchronization would not solve those costs.

## Decision ledger

- **Decision:** Retain verified Git bundles and private guest repositories.
- **Security and checkout confidence:** High.
- **Incremental-bundle confidence:** Medium until update and result prerequisites, failure recovery, and real size reduction are demonstrated.
- **Next action:** Benchmark create, update, and fetch for four concurrent workspaces seeded from one exact commit. Record bundle production, runtime copy, guest checkout, dependency setup, and project-lock wait separately.
- **Prototype trigger:** Bundle production or copy is a material part of that breakdown.
- **Revisit triggers:**
  - The selected reachable ref approaches or exceeds 512 MiB.
  - Bundle or lock work dominates parallel create, update, or fetch.
  - Users require uncommitted or non-Git source fidelity.
  - GitHub becomes the required source authority.
  - Apple adds supported named-volume snapshot, clone, or import APIs.
- **Revisit date:** After the benchmark or the next Apple `container` compatibility upgrade, whichever occurs first.
