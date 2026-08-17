# Repository Guidelines

## Project Overview

DSX is a daemonless Go CLI for creating secure Linux development workspaces on Apple-silicon Macs with Apple's `container` runtime. Every workspace is a named peer backed by a guest-owned private Git clone. DSX supports four coding-agent harnesses, a locally built pinned-input non-root DSX Standard image, isolated project/workspace/session authentication, opt-in per-workspace access to the host AWS `default` profile, project processes and services, loopback port publication, and a disposable Playwright browser for an explicitly browser-enabled agent session.

Product requirements and architecture are authoritative in `docs/PRD.md` and `docs/adr/0001-dsx-implementation-architecture.md`. Treat `docs/plans/implementation-plan.md` as execution history beneath those contracts, not permission to reduce scope. `docs/README.md` defines the remaining documentation hierarchy and precedence.

## Architecture & Data Flow

Dependency direction:

```text
cmd/dsx composition root
  -> internal/hostcmd and internal/tui presentation
  -> internal/app use-case services
  -> domain ports in plan/state/runtime/harness/gitx/auth/bridge
  -> adapters such as runtime/apple, state/fs, guest, and harness/*
```

Primary flow:

1. `internal/inspect` discovers canonical project facts without mutation.
2. `internal/config` parses bounded JSONC against the embedded offline schema.
3. `internal/plan` resolves config/import/CLI/default precedence into a deterministic plan and executable hash.
4. `internal/state` authorizes the exact hash and persists write-ahead manifests before runtime mutation.
5. `internal/app` coordinates named-workspace lifecycle, Git source/update/result transfer, agent, authentication, browser-session, port, and bridge operations through injected interfaces.
6. `internal/runtime/apple` invokes Apple `container` with structured argv and re-inspects ownership before mutation.
7. `internal/app/guest_client.go` communicates with `cmd/dsx-guest` using the bounded, versioned `internal/guestproto` protocol.

The local checkout is the source and integration point, not a DSX workspace. Clean committed ingress is the default; explicit reviewed final-working-tree snapshot ingress is the alternative for tracked changes and nonignored untracked files. Both transfer bounded verified Git bundles into guest-owned private clones without shared Git objects or host source/home mounts. Every lifecycle and Git operation names its workspace; workspace, agent, authentication, and browser-session lifecycles remain separate. Results return through verified bundles and guarded fetch/apply transactions. A browser receives only the selected workspace's private network—never source, auth, AWS, host paths, or published host control ports—and is deleted with its agent session.
`--snapshot` is per-create or per-update invocation state, not configuration authority or an approval-hash input. It transfers final working-tree bytes for every tracked path plus nonignored untracked paths; ignored untracked paths remain excluded, and staged/unstaged distinctions are flattened. Capture must reject unmerged paths, gitlinks, embedded repositories, unsafe paths, unsupported file types, bounds violations, and observable races. It may use only private temporary indexes, object quarantines, refs, and bundles and must not mutate the host branch, `HEAD`, index, worktree, durable refs, configuration, hooks, or object database.

Interactive onboarding is intentionally narrow: choose Ubuntu default or custom settings, review the complete effective authority, then run bounded setup progress. The default is Codex with 6 CPUs, 6 GiB, internet access, no published ports, and no browser session. The setup and dashboard layouts reflow for wide, narrow, and short terminals, respect `DSX_ACCESSIBLE=1` and `NO_COLOR`, wrap by terminal cells, and expose only state-valid actions. The dashboard's experimental VS Code action prints inspected attachment guidance for a running workspace; it does not parse `.devcontainer` or attach automatically. TUI models emit intents only; `internal/hostcmd` owns workspace-creation progress and the direct terminal handoff for **Create and open a shell**.

## Key Directories

- `cmd/dsx/`: Darwin/ARM64 host CLI and production dependency composition.
- `cmd/dsx-guest/`: Linux/ARM64 guest helper and narrow control/staging commands.
- `internal/app/`: application services for inspect, setup, named-workspace lifecycle, Git transfer, agents, authentication, per-workspace AWS grants, browser sessions, and bridges.
- `internal/hostcmd/`: explicit CLI parsing, rendering, exit codes, and TTY dispatch.
- `internal/tui/`: Bubble Tea/Huh presentation; models emit intents and do not implement lifecycle transitions.
- `internal/config/`, `internal/plan/`, `internal/model/`: validated configuration, deterministic execution plans, IDs, states, and typed errors.
- `internal/runtime/`, `internal/runtime/apple/`: runtime contracts and Apple adapter.
- `internal/state/`, `internal/state/fs/`: approvals, manifests, locks, atomic persistence, and optimistic generations.
- `internal/guest/`, `internal/guestproto/`: guest supervisor/server and bounded host–guest protocol.
- `internal/harness/`, `internal/auth/`: harness adapters and attestation; canonical project credentials, persistent workspace credentials, exclusive session copies, promotion, conflicts, and purge.
- `internal/gitx/`: hardened Git identity/configuration, clean and snapshot source bundles, quarantined synthetic objects, result bundles, diff/fetch, and transactional apply logic.
- `internal/bridge/`, `internal/ports/`, `internal/terminal/`: leased host bridges, publication planning, PTY handoff, signals, and safe rendering.
- `internal/ownership/`, `internal/hostsource/`, `internal/hostopen/`: ownership proofs, denied-host-path overlap checks, and bounded host URL/callback handling.
- `schema/`: strict embedded JSON Schema; keep it aligned with `internal/config` types and validation.
- `images/agent/`, `images/browser/`: embedded digest/checksum-pinned OCI recipes, harness and shell-toolchain locks, non-root shell assets, and the disposable browser server.
- `tests/contract/`: cross-package, artifact, workflow, schema, and import-boundary contracts.
- `tests/apple/`: opt-in destructive tests against the real Apple runtime.
- `scripts/release/`: reproducible candidate build, SBOM, signing, notarization, and verification; never publishes.
- `scripts/runner-ops/`: physical-runner inventory, ledger, exact cleanup, sweep, evidence, and quarantine.
- `docs/adr/`, `docs/plans/`, `docs/research/`, `docs/reviews/`: authoritative architecture decisions, non-authoritative execution plans, and dated supporting analysis; see `docs/README.md` for precedence.
- `docs/manual/`: user-facing onboarding and detailed user/operator behavior.

## Development Commands

Use Go 1.26.5 for reproducible development.

```bash
make build          # bin/dsx + bin/dsx-guest
make build-host     # Darwin/ARM64 host CLI
make build-guest    # static Linux/ARM64 guest helper
make test           # go test ./...
go test ./internal/app -run '^TestName$' -count=1
go test -race ./internal/app ./internal/state ./internal/ownership
go vet ./...
gofmt -w <touched-go-files>
./bin/dsx help
./bin/dsx doctor
```

Verify the guest dependency boundary after guest changes:

```bash
go test ./tests/contract -run '^TestGuestImportClosure$' -count=1
```

There is no repository-specific linter or schema generator. Do not invent `golangci-lint`, GoReleaser, npm, or generation steps for ordinary Go changes.

## Code Conventions & Common Patterns

- Follow standard Go formatting and naming. Exported contracts use domain nouns such as `Adapter`, `Repository`, `Service`, `Request`, `Result`, `Spec`, `Plan`, and `Record`.
- Use constructor injection (`NewX`, `NewXWithDependencies`) and small interfaces/functions at the consumer boundary. Avoid globals and broad service locators.
- Put `context.Context` first on operations. Use deadlines/select loops; do not introduce unbounded sleeps or detached goroutines.
- Classify errors with `internal/model.NewError` or `model.Wrap`; preserve causes with `errors.Is/As`. Join rollback/cleanup failures with `errors.Join` rather than dropping the original failure.
- Represent subprocess calls as executable plus structured argv/env. Never interpolate project input into an implicit host shell.
- Keep I/O bounded and deterministic. Sort collections that affect hashes, manifests, rendering, evidence, or helper specifications.
- Keep secrets out of plans, hashes, manifests, logs, errors, TUI content, process listings, and bridge status. Stage secret values through private bounded files.
- Persist authorization and planned manifest intent before runtime mutation. Revalidate plan hash, ownership labels, physical paths, bundles, DNS, helper/image identity, and guest generation immediately before sensitive use.
- Run host Git with the hardened environment and inert repository-local configuration allowlist in `internal/gitx`; never permit repository hooks, command-bearing filters, include directives, credential helpers, executable transports, or ambient protocol configuration to bypass that boundary.
- AWS is disabled for every new workspace. A project-approved `host-default` capability and a separate durable workspace grant are both required; publish only atomic, read-only `default` generations to `/run/dsx/aws`, revoke stable absence, and never expose AWS material to browser sessions.
- Never delete by resource name alone. Ambiguous ownership is reported and preserved.
- Preserve lock order: workspace lock before project lock. Preserve legal transitions in `internal/model/state.go` and optimistic manifest generations.
- Cancellation stops forward work; rollback and result/auth capture use a bounded independent context when required. Cleanup is idempotent and must preserve unfetched or uncertain work.
- Goroutines are reserved for real concurrent I/O/lifecycle work: PTYs, process supervision, Unix connections, health checks, and relay copying. Protect shared state with mutexes and signal completion through channels/context.
- TUI models return intents only. Sanitize repository/runtime text with `internal/terminal`; restore terminal state around interactive child handoff and forward resize/signals.
- Preserve the Standard image's `dsx` user/group contract at UID/GID 1000 with passwordless `sudo`; do not silently replace or recursively re-own existing workspace storage when the image changes.

## Important Files

- `cmd/dsx/main.go`: production composition root and hidden leased-helper modes.
- `internal/hostcmd/execute.go`: command surface and dependency interfaces.
- `internal/app/workspace.go`: named-workspace create/open/start/stop/restart/remove/list transactions and ownership-safe rollback.
- `internal/app/workspace_update_rebase.go`, `internal/app/workspace_git.go`: committed or snapshot source update/rebase and guarded Git result recovery.
- `internal/app/harness.go`, `internal/app/auth.go`, `internal/app/browser.go`: workspace-targeted agent sessions, canonical/project credential handling, isolated copies, and disposable per-session browsers.
- `internal/app/aws.go`, `internal/bridge/leapp_mirror_manager.go`: workspace AWS grant lifecycle and continuous bounded default-profile mirroring.
- `internal/gitx/source.go`, `internal/gitx/quarantine.go`, `internal/gitx/apply.go`: clean/snapshot ingress, host-object quarantine, and guarded result application.
- `internal/tui/model.go`, `internal/tui/dashboard.go`, `internal/tui/style.go`: intent-only setup/dashboard models, responsive layout, accessibility, and terminal-cell rendering.
- `images/agent/assets.go`, `images/agent/shell-toolchains.lock.json`, `images/agent/harnesses.lock.json`: embedded Standard-image authority, pinned developer toolchains, and exact harness build attestation.
- `internal/plan/hash.go`: reusable project-default executable projection and approval-hash contract.
- `internal/state/manifest.go`: durable workspace ownership/resource/Git record validation.
- `internal/runtime/runtime.go`: DSX runtime port; `internal/runtime/apple/adapter.go` is its Apple implementation.
- `schema/dsx-config-v1.schema.json`: manually maintained strict Draft 2020-12 schema.
- `images/agent/harnesses.lock.json`: exact-byte harness lock coupled to the agent recipe and attestation digest.
- `.github/workflows/ci.yml`: ordinary unit, race, fuzz, cross-build, and import-boundary CI.
- `.github/workflows/apple-physical.yml`: protected physical macOS 26/27 evidence lane.
- `docs/PRD.md`, `docs/adr/0001-dsx-implementation-architecture.md`: normative product and architecture contracts.
- `docs/manual/getting-started.md`, `docs/manual/user-guide.md`: current user-facing documentation.

## Runtime/Tooling Preferences

- Module: `github.com/srimajji/dsx`.
- `go.mod` has language floor 1.25.8 and selects toolchain Go 1.26.5; CI pins 1.26.5.
- Target binaries are CGO-disabled Darwin/ARM64 `dsx` and Linux/ARM64 `dsx-guest`.
- The host runtime is Apple `container`; Docker and Podman are not runtime dependencies. Never expose any runtime socket to a guest.
- Physical runtime support is macOS 26+ on Apple silicon with the tested Apple CLI/API-server pair. Ordinary CI must not probe or mutate an installed runtime.
- Bubble Tea/Huh are presentation adapters. JSONC and the embedded schema define project configuration.
- Node/npm are needed only when refreshing browser package assets. OCI image building is required when rebuilding the agent or browser images and when runtime setup prepares DSX Standard. Syft, Developer ID credentials, and notarization tooling are release-only.
- Keep `go.mod`/`go.sum`, browser `package.json`/`package-lock.json`, and schema/config types synchronized.
- Harness upgrades are atomic across `images/agent/harnesses.lock.json`, `images/agent/Containerfile`, adapter constants, and `internal/harness/attestation.go`. Even lockfile whitespace changes its attested digest.
- DSX Standard image changes must keep `images/agent/Containerfile`, `images/agent/assets.go`, `images/agent/shell-toolchains.lock.json`, shell/sudo assets, lock contract tests, and the executable-plan image authority synchronized.
- Keep GitHub Actions and remote artifacts pinned by immutable SHA/digest. Generated `bin/`, `dist/`, and `.release-tools/` output stays uncommitted.
- `.github/workflows/pullfrog.yml` is manually dispatched external-agent automation, not ordinary CI, release, or physical-runtime evidence. Keep its permissions and provider-secret exposure minimal; the immutable-action-pin rule still applies.

## Testing & QA

Tests use the standard `testing` package, same-package fakes, table tests, `t.TempDir`, `t.Cleanup`, and explicit contexts. Prefer tests that defend observable contracts and plausible failure modes: ordering, durable state, exact argv/env, filesystem permissions, deterministic output, error codes, rollback, and non-mutation.

- Package-local `*_test.go`: deterministic unit/integration tests with recording fakes.
- `tests/contract/`: cross-package and repository artifact contracts.
- `tests/apple/`: real runtime compatibility, lifecycle, fault cleanup, and performance.
- `testdata/config/` and `internal/inspect/testdata/`: parser and heterogeneous repository fixtures.

Run focused tests while iterating, then `go test ./...`; use `go test -race` for stateful/concurrent packages. Add fuzz seeds for parser/protocol/terminal/ownership/runtime/bridge inputs when changing those boundaries. Do not add `t.Parallel` to tests that mutate globals, environment, shared process state, or real runtime resources.
After `AGENTS.md` or named-workspace documentation changes, run `go test ./tests/contract -run '^TestMaintainerGuideUsesNamedWorkspaceArchitecture$' -count=1`. After Standard-image input changes, run `go test ./images/agent ./tests/contract -run '^(TestMaterialize|TestInputDigest|TestHarnessImageLock|TestShellToolchainsLock)' -count=1` in addition to the relevant build or runtime smoke check.
For every TUI or onboarding change, MUST build the host binary and manually sanity-check the real UI end to end on a safe isolated project: start from bare `dsx`, complete every onboarding and approval step, continue through successful workspace creation and attach, and verify Apple container-system and builder prerequisites along the way. Unit tests, render snapshots, or stopping at the final confirmation screen are not sufficient verification.


Never casually enable destructive Apple tests. `DSX_RUN_APPLE_TESTS=1` is for a dedicated physical Apple-silicon host. Fault and performance suites require additional evidence/run variables and ownership-safe recovery. They must inventory unrelated resources, use unique labeled run IDs, clean exact proven resources twice, preserve the Apple builder/default network, and quarantine on ambiguity—never prune broadly.

When an observable CLI/config/security contract changes, update the relevant tests and `docs/manual/`. Change the PRD for product requirements, the ADR for architecture decisions, and `docs/operations/runner-operations.md` plus runner schemas/scripts together for physical-runner protocol changes. Implementation or local observation alone is not release evidence.
