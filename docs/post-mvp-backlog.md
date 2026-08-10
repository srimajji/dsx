# DSX MVP closeout and post-MVP backlog

- **Status:** Living backlog after MVP implementation
- **Date:** 2026-08-10
- **Authority:** [PRD v0.3](./PRD.md) and [ADR 0001](./adr/0001-dsx-implementation-architecture.md)
- **Completed execution plan:** [DSX MVP implementation plan](./implementation-plan.md)

## 1. Purpose and status language

The MVP implementation is present in the repository. This does not mean DSX is a signed, published, or fully release-certified product. This document separates:

- **Missed or blocked MVP evidence:** behavior or release proof required by the MVP contract but not completed because a prerequisite, identity, credential, fixture, or physical lane was unavailable.
- **Skipped by design:** work deliberately not exposed because its safety or vendor contract was not proven, or because the MVP selected a narrower behavior.
- **Post-MVP candidates:** features discussed in the PRD, ADR, implementation plan, or implementation review that were not part of the completed MVP contract.

A source implementation, fake-provider test, or local macOS 27 experiment must not be promoted to release support without the evidence named below.

## 2. What the MVP delivered

All implementation tasks DSX-001 through DSX-085 are represented by production code, tests, workflows, release tooling, or operator documentation. The completed repository includes:

- Darwin/arm64 `dsx` and static Linux/arm64 `dsx-guest` binaries.
- Inspection, JSONC/schema validation, provenance, executable-plan hashing, exact approval, doctor, deterministic CLI commands, and the context-aware TUI.
- Live workspaces and named clone sandboxes on Apple `container`, including manifests, labels, ownership classification, rollback, idempotent cleanup, PTY handling, and guest process supervision.
- Fixed and fallback-reserved dynamic loopback ports. Apple 1.2.2 native `:0` publication remains classified as unsupported.
- OMP, Codex, Claude, and OpenCode adapters; isolated authentication stores; MCP injection contracts; login orchestration; and fail-closed image/harness attestation.
- Git bundle creation, clone isolation, status, diff, fetch, guarded apply, and unfetched-result protection.
- Disposable zero-mount browser resources, private-network ownership, local browser-isolation proof, Leapp mirroring with local atomic-generation proof, and destination-specific private relay implementation.
- Ordinary CI, physical-runner protocol, fault/race/fuzz/performance suites, packaging/SBOM/signing/notarization gates, operator recovery docs, and user documentation.

At closeout, project-wide formatting, `go vet ./...`, `go test -race ./...`, and both cross-builds passed. Local Apple 1.2.2 scenarios on macOS 27 exercised lifecycle cleanup, loopback port fallback, named-clone Git flows, TUI PTY paths, browser isolation, Leapp generation switching, and the available OpenCode one-shot path. The limitations below remain authoritative.

## 3. Missed or blocked MVP completion evidence

| ID | Missing evidence or prerequisite | Current state | Closure criterion |
|---|---|---|---|
| MVP-GAP-001 | Immutable reference workspaces | No pinned, nonsecret `course-intelligence-agency` or `devenv` snapshots are committed under `testdata`; complete cross-language application/HMR and composite-service acceptance therefore remains unrepeatable. | Pin legal nonsecret snapshots or dedicated immutable checkouts, record their digests, and run the documented live, clone, service, port, browser, Git, and cleanup scenarios. |
| MVP-GAP-002 | Physical macOS 26 and 27 lanes | Workflow and runner-operation contracts exist, but dedicated runners, canary, sweeper, quarantine drill, and recovery drill have not been provisioned and reviewed. | Run the full protected physical workflow on both supported macOS majors; retain host/runtime attestation, ledgers, sentinels, performance evidence, repeated cleanup, and builder/baseline equality. |
| MVP-GAP-003 | Real harness authentication and PTY portability | Adapter, fake-provider, credential-copy, conflict, redaction, signal, and resize contracts exist. Real OMP/Codex/Claude credentials and complete provider flows were unavailable; OpenCode one-shot observation does not close authentication persistence for every harness. | For every pinned harness, prove interactive and one-shot execution, login, recreation, credential refresh, concurrent-copy conflict behavior, MCP precedence, signals, resize, purge, and secret-free output on a physical lane without purging existing user credentials. |
| MVP-GAP-004 | Provider-specific OAuth callback support | The bounded callback lease and validated URL opener exist, but no pinned harness exposes a verified caller-supplied redirect URI plus guest-delivery contract. Application wiring remains fail closed rather than fabricating vendor acceptance. | Obtain a version-pinned vendor contract and real test credentials; prove exact redirect/callback delivery, state validation, expiry, duplicate rejection, redaction, and cleanup before wiring it into a public harness flow. |
| MVP-GAP-005 | Private relay end-to-end proof | Live and clone lifecycle wiring, immutable destination policy, lease ownership, and focused tests exist. A real private route was not available for destination-abuse, interface-binding, cross-sandbox, sleep/wake, and crash-expiry evidence. | Exercise an approved private TCP endpoint on dedicated runners; prove only the pinned destination works, the listener is not LAN/tailnet exposed, other sandboxes fail, and every stop/crash/expiry path removes the helper. |
| MVP-GAP-006 | Production registry and image publication | Agent/browser recipes and lock/attestation contracts exist; production registry ownership and published digest-pinned image references were not supplied. Development-only `dsx.local` references are not release authority. | Choose the registry owner, build and publish the pinned Linux/arm64 images, record immutable references and provenance, and pass clean pull plus byte/digest attestation. |
| MVP-GAP-007 | Apple signing and notarization identity | Release scripts fail closed and unsigned dry-run artifacts can be generated. No Developer ID Application identity or `notarytool` Keychain profile was supplied. | Sign with the approved authority and timestamp, notarize to `Accepted`, staple/assess with Gatekeeper, and verify from a clean installation prefix. |
| MVP-GAP-008 | Release-candidate evidence bundle | Packaging, SBOM, integrity, runner evidence, and strict verification tooling exist, but their external inputs and the preceding gates are incomplete. | Complete MVP-GAP-001 through MVP-GAP-007, run the release workflow without bypasses, obtain independent security sign-off, and archive the complete evidence matrix. |

These are completion-evidence gaps, not permission to add fake success paths or weaken fail-closed behavior.

## 4. Work deliberately skipped or narrowed in the MVP

| ID | Skipped or narrowed work | Reason and current replacement |
|---|---|---|
| MVP-SKIP-001 | Apple 1.2.2 native dynamic `127.0.0.1:0:<guest>` publication | The reachability experiment failed, so capability probing keeps native publication disabled. DSX uses the bounded loopback reservation/handoff fallback and treats runtime inspect as authoritative. |
| MVP-SKIP-002 | Public ambient-host `dsx auth import` | Arbitrary OMP databases and macOS Keychain-backed Claude credentials are not safely portable. DSX accepts only adapter-declared artifacts through explicit login/profile flows. |
| MVP-SKIP-003 | Generic auth-conflict merge command | Concurrent refresh conflicts preserve a DSX-owned candidate and leave the active seed unchanged. Generic JSON/SQLite merging could corrupt credentials; users perform a fresh explicit login instead. |
| MVP-SKIP-004 | Public OAuth callback wiring | Kept internal and fail closed until MVP-GAP-004 is satisfied. The implementation does not pretend that a provider accepts DSX's callback. |
| MVP-SKIP-005 | General process-control surface | The MVP exposes read-only `status` and bounded live-workspace `logs PROCESS`. It deliberately omits `dsx ps`, arbitrary `dsx exec`, log following, and named-clone process selection because these need new authorization and lifecycle contracts. |
| MVP-SKIP-006 | Automated publication | Release tooling builds and verifies but does not publish. Registry, signing, notarization, physical acceptance, and human release ownership remain explicit gates. |
| MVP-SKIP-007 | Native Docker/Compose/Testcontainers adoption | DSX imports only allowlisted declarative project facts and never exposes a Docker/Podman/runtime socket to a workspace. Unsupported lifecycle semantics fail rather than being inferred or executed. |

## 5. Post-MVP feature candidates discussed

These are candidates, not commitments. Each requires PRD prioritization and, where it changes a locked decision, an ADR update before implementation.

| ID | Candidate | User value | Required decision or safety proof |
|---|---|---|---|
| POST-MVP-001 | Docker/Podman compatibility backend | Broader repository compatibility where Apple-native execution is not viable. | Define a separate runtime contract and prove that no control socket enters the agent workspace; do not silently change the Apple-native default. |
| POST-MVP-002 | One sibling Apple container per service | Stronger service isolation and independent service lifecycle. | Measure startup/memory cost, define cross-container network and ownership semantics, and compare against the one-integrated-VM ADR decision. |
| POST-MVP-003 | Rich process operations | Process inventory, named-clone log selection/following, and tightly scoped interactive execution. | Define process identities, authorization, bounded output, PTY ownership, clone selectors, and cleanup behavior; arbitrary runtime exec is not an acceptable shortcut. |
| POST-MVP-004 | Authentication portability and conflict UI | Easier profile import/export, provider migration, and conflict-candidate resolution. | Require adapter-specific portable formats, closed-snapshot rules, exact ownership, redaction, and no generic secret merge. |
| POST-MVP-005 | GUI or web control surface | Richer multi-sandbox monitoring and onboarding. | Preserve application-service reuse, browser/source trust boundaries, local authentication, terminal handoff, and daemonless behavior unless a separate daemon ADR is approved. |
| POST-MVP-006 | Optional background scheduler/daemon | Queued work, monitoring, and recovery without an attached terminal. | New threat model, install/update/uninstall lifecycle, idle-resource budget, authenticated IPC, crash recovery, and ownership model; the current daemonless invariant remains until then. |
| POST-MVP-007 | Explicit host-service bridge | Reuse selected host databases or development services when duplication is impractical. | Permit only typed destination grants with clear mutation authority; never adopt ambient host loopback or expose a generic proxy. |
| POST-MVP-008 | Additional Apple and harness versions | Support newer Apple 1.2.x patches and independent harness upgrades. | Treat every runtime patch and harness artifact as a compatibility event with pinned provenance, canary evidence, and no widening of the current allowlist by version range alone. |

## 6. Explicitly not carried into the backlog

The following remain product non-goals or rejected security boundaries, not hidden future commitments: Kubernetes, nested containers, Rosetta/amd64 execution, arbitrary Nix or host shell interpretation, generic VPN/SOCKS/CONNECT proxying, prompt coordination, merge automation, remote execution, host Git worktrees as the sandbox boundary, complete host-home mounts, agent access to runtime control sockets, and deletion/adoption of unrelated Apple resources.

Moving any of these into product scope requires an explicit PRD change and usually a new ADR.

## 7. Recommended closure order

1. Secure production registry ownership, pinned image references, signing identity, and notarization credentials.
2. Pin the two reference workspaces and provision protected macOS 26/27 physical runners.
3. Close real harness authentication/PTY, private relay, browser, Leapp, lifecycle, Git, and performance evidence on those lanes.
4. Produce, independently review, and retain the complete release-candidate evidence bundle.
5. Only after the MVP release gate closes, prioritize post-MVP candidates against measured demand and the security cost of changing locked architecture.
