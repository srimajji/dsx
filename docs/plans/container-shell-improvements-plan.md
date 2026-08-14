# Container shell improvements

## Context
DSX Standard currently has three conflicting identities: the image declares `dsx` as `501:20`, DSX executes normal children as numeric `1000:1000`, and workspace creation records the container init user as `0:0`; VS Code Dev Containers 0.467.0 Apple attach inherits that recorded init user and therefore attaches as root. Standard-image workspaces also have no `sudo`, use digest-pinned Ubuntu 24.04, and omit AWS CLI v2, uv, .NET, and Kotlin. The target is a stable `dsx` account at `1000:1000`, passwordless VM-local elevation, a digest-pinned Ubuntu 26.04 LTS Standard image with reproducibly pinned AWS CLI v2 2.36.22, uv 0.12.3, .NET 10 LTS SDK 10.0.400, and Kotlin 2.4.10, plus an explicit truthful TUI action for Microsoft's experimental Apple-container attach flow without reading or executing Dev Container declarations or relying on Microsoft's private remote-authority encoding.

## Approach

1. Define the product and architecture contract before changing implementation.
   - In `docs/PRD.md` R5 and §8.2, specify that DSX Standard uses Ubuntu 26.04 LTS ARM64; its development identity is exactly `dsx` (`UID 1000`, `GID 1000`, home `/home/dsx`, login shell `/bin/zsh`); direct user shells and IDE attachment may run passwordless `sudo` inside the workspace VM; there is no root password and direct root login is not a user workflow. Retain the existing statement that elevation exposes the entire workspace VM and mounted workspace resources but no host runtime/home/source authority.
   - In R5, add these exact Standard-image guarantees: AWS CLI v2 2.36.22 (`aws` and `aws_completer`), uv 0.12.3 (`uv` and `uvx`), .NET 10 LTS SDK 10.0.400 including .NET/ASP.NET Core runtimes 10.0.11 (`dotnet` and `dnx`), and standalone Kotlin compiler 2.4.10 (`kotlin` and `kotlinc`) on the existing Temurin JDK. State that AWS CLI installation grants no AWS capability, Python `awscli` v1 is not installed, Kotlin does not imply Gradle/Maven/Kotlin Native, and project dependency resolution remains explicit.
   - In `docs/adr/0001-dsx-implementation-architecture.md` §18 and Guest boundary, record the privilege split: Apple records container user `1000:1000`; unprivileged `dsx-guest serve` supervises normal children at the same identity; the host invokes one narrowly validated root-only `dsx-guest initialize-workspace` operation immediately after start to set owned-volume roots; managed guest processes keep `PR_SET_NO_NEW_PRIVS`, while direct `container exec` shells and VS Code may use image-provided passwordless sudo.
   - Add an External IDE subsection to both contracts: `[v] Attach with VS Code` is experimental, available only for a definitely running, ownership-verified workspace, opens the documented VS Code setting, and prints the exact documented Command Palette/picker steps and DSX container name. It never parses `.devcontainer/**`, starts a stopped workspace, creates a remote server itself, publishes a private `apple-container+...` URI, or changes `dsx workspace open NAME` from a shell operation.

2. Rebuild the managed image contract on Ubuntu 26.04 LTS without weakening reproducibility.
   - Replace both `FROM` references in `images/agent/Containerfile` and `base.reference` in `images/agent/harnesses.lock.json` and `images/agent/shell-toolchains.lock.json` with the official Ubuntu 26.04 ARM64 digest `docker.io/library/ubuntu@sha256:3fe5b610f5c41eeeb56c2995bd4afb4990ac5b80dc980e33f9251eaaa8013615` (Docker Hub `ubuntu:26.04`, published 2026-08-04). Add lock metadata `release: "26.04"` and assert it in contract tests so the digest remains human-auditable; never use floating `ubuntu:26.04` in `FROM`.
   - Keep `https://snapshot.ubuntu.com/ubuntu/20260811T000000Z/`. Resolve every existing apt package plus `sudo` against the Resolute ARM64 indexes at that snapshot, replace every Noble version in the lock and recipe with the exact resolved version, and update tool-provider versions derived from those packages. Fail rather than dropping a package or relaxing `name=version` pins if the snapshot does not contain one.
   - Extend `shellToolchainsLock.Artifacts` in `tests/contract/harness_image_test.go` and artifact objects in `images/agent/shell-toolchains.lock.json` with optional source-provenance fields `upstreamDigestAlgorithm`, `upstreamDigest`, `signature`, and `signingKeyFingerprint`. Keep the existing required `sha256` as the exact BuildKit content pin. Require the .NET entry to contain algorithm `sha512` plus its 128-character upstream digest; require the AWS entry to contain both its `.sig` URL and 40-hex signing fingerprint; reject provenance fields on other artifacts, unsupported algorithms, malformed digest lengths, incomplete signature metadata, and source/signature mismatches.
   - Add checksum-pinned artifact `aws-cli-v2` version `2.36.22`, source `https://awscli.amazonaws.com/awscli-exe-linux-aarch64-2.36.22.zip`, install root `/opt/aws-cli`, signature URL equal to the source plus `.sig`, and signing-key fingerprint `FB5DB77FD5C118B80511ADA8A6310ACC4672475C`. Verify the exact ZIP against AWS's documented key and signature before calculating and recording its lowercase SHA-256; never fabricate an AWS-published SHA-256. Extract offline in the artifact stage, run only the archive's local `aws/install --install-dir /opt/aws-cli --bin-dir <staging-bin>`, preserve its notices, copy the versioned tree, and create final links `/usr/local/bin/aws` and `/usr/local/bin/aws_completer`. Add exact snapshot-pinned `groff` alongside existing `less`; do not install apt/pip `awscli`.
   - Add checksum-pinned artifact `uv` version `0.12.3`, source `https://github.com/astral-sh/uv/releases/download/0.12.3/uv-aarch64-unknown-linux-musl.tar.gz`, SHA-256 `fa513fca1eb2913334c944fe9adbdd410274a1cbe8dd05d03699a9eb85311d4e`, install root `/usr/local/bin`. Extract the static ARM64 archive with one stripped top-level directory and install `uv` and `uvx` mode `0755`; do not run Astral's installer or invoke uv at shell startup.
   - Add artifact `dotnet-sdk` version `10.0.400`, source `https://builds.dotnet.microsoft.com/dotnet/Sdk/10.0.400/dotnet-sdk-10.0.400-linux-arm64.tar.gz`, install root `/opt/dotnet`, upstream digest algorithm `sha512`, and upstream digest `a1b45da58e5591fff909a6126ac6bfc1ef9c12bc72c0625f7815e83a82be1a902317ee96926cbbf81324a45c6abf2ed8102a216d0507879cc166159af78d1b77`. Verify Microsoft's SHA-512 first, then record the computed SHA-256 for `ADD --checksum`; extract without stripping. Install exact snapshot pins for its native dependencies. Export `DOTNET_ROOT=/opt/dotnet` and prepend `/opt/dotnet:/opt/dotnet/tools` to image-level `PATH`; do not use APT `dotnet-sdk`, Microsoft's install script, or separate runtime archives.
   - Add checksum-pinned artifact `kotlin` version `2.4.10`, source `https://github.com/JetBrains/kotlin/releases/download/v2.4.10/kotlin-compiler-2.4.10.zip`, SHA-256 `473dd66c7a3ef4b182065b3da670466c1bf2773a9dbb0ed8b33a39fe9d4f876d`, install root `/opt/kotlin`. Extract with the pinned JDK's `jar`, rename top-level `kotlinc` to `/opt/kotlin`, preserve licenses, and retain executable launchers. Export `KOTLIN_HOME=/opt/kotlin` and add `/opt/kotlin/bin` to image-level `PATH`.
   - Add lock tool and environment entries for all executables and exact PATH order `/opt/dotnet:/opt/dotnet/tools:/opt/kotlin/bin:/opt/node/bin:/opt/go/bin:/opt/java/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`.
   - Replace the image account with dedicated `dsx` group/account `1000:1000`; set `HOME`, `USER`, `LOGNAME`, `SHELL`, and final `USER 1000:1000`.
   - Add `images/agent/sudoers-dsx` with exact policy `dsx ALL=(ALL:ALL) NOPASSWD: ALL`, mode `0440`, and build-time `visudo` validation. Embed it so bytes affect the managed-image digest.
   - Create `/run/dsx` and private-volume mount targets owned by `1000:1000`; extend strict image contract tests and negative installer/security assertions.

3. Make the container's recorded default user non-root while preserving the least privileged initialization exception.
   - Replace injectable/host-derived identities with package constant `standardWorkspaceUser = "1000:1000"` across workspace operations and guest control.
   - Record `WorkspaceSpec.User` as `1000:1000`; run `dsx-guest serve` unprivileged with matching child IDs and without initialization.
   - Add exact root-only `dsx-guest initialize-workspace` with bounded ordered paths `/workspace`, `/home/dsx/.dsx/auth`, `/home/dsx/.local/state/dsx`, `/home/dsx/.cache`, `/var/lib/dsx`.
   - Generalize descriptor/no-follow initialization: mode `0700`, empty `lost+found` removal only, `fchown`, postcondition verification, no traversal or recursive ownership changes.
   - After start and ownership re-inspection, execute only that initializer as `0:0`, then wait for readiness and bootstrap. Permit only this narrow root exec in the Apple adapter.
   - Update focused tests to prove the recorded user, supervisor identity, five initialized roots, root rejection, and fixed identity for every normal operation.

4. Add an ownership-verified, experimental VS Code attach action at the presentation boundary.
   - Add read-only `WorkspaceService.AttachInfo` returning only the workspace and exact observed owned running container.
   - Add injected `VSCodeLauncher.OpenSettings`; production invokes `/usr/bin/open vscode://settings/dev.containers.experimentalAppleContainerSupport` with structured argv.
   - Add TUI action `vscode-attach`, key `v`, shown only for running non-mutating workspaces, including accessible mode.
   - Resolve attach info before opening settings, then print sanitized documented picker guidance. Do not emit private authority URIs, parse `.devcontainer`, start workspaces, or add OSC 8 output.
   - Preserve create-and-open PTY handoff ordering; add post-background/post-shell guidance and focused tests.

5. Update user-facing behavior and upgrade guidance.
   - Document identity, sudo scope, exact baseline tool versions and limitations, Ubuntu 26.04, experimental VS Code picker flow, explicit port publication, and immutable old-workspace migration through preserve/remove/reapprove/recreate.

## Verification

- Format touched Go files; run focused package and contract tests, guest import closure, and `make build`.
- Run `go test ./...`, the specified race tests, and `go vet ./...`.
- On the physical Apple runtime, use `/Volumes/Dev/work/course-intelligence-agency` without changing its ahead commit or working tree. Verify onboarding through shell handoff, identity and sudo, every pinned tool, Kotlin/.NET probes, CIA dependency install/build/test/lint/dev ports, managed-process no-new-privileges, restart persistence, experimental VS Code attach, stopped-action gating, and unchanged repository state.
- Inventory resources before the run; remove only exact disposable DSX resources twice; preserve unrelated resources and Apple builder/default network.

## Assumptions and contingencies

- Ubuntu 26.04 LTS OCI, not Ubuntu Core appliance OS.
- Fixed guest identity, not copied host identity.
- Passwordless sudo is scoped to the isolated workspace VM; no host runtime socket or host mounts are granted.
- Tool versions are frozen at the 2026-08-13 planning cutoff.
- AWS and Microsoft publisher proof must authenticate bytes before BuildKit SHA-256 pins are recorded.
- VS Code support remains picker-based and experimental until Microsoft publishes a stable external target contract.
