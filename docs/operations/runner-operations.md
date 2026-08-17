# Physical Apple runner operations

## Status and scope

This guide defines the DSX-084 operating contract for destructive Apple-runtime CI. **No physical macOS 26 or macOS 27 runner has been provisioned, registered, or exercised by this repository change. No canary, destructive suite, sweeper, quarantine drill, or recovery drill has run here.** The workflow is therefore a fail-closed configuration awaiting separately approved physical hosts and observed evidence.

The workflow is `.github/workflows/apple-physical.yml`. The machine-readable provisioning reference is `runner-ops/physical-apple.example.json`. The scripts under `scripts/runner-ops` implement the in-job lock, inventory, ledger, sentinel, sweeper, cleanup, and evidence protocol. They do not provision hosts or change runner-group membership.

## Required GitHub controls

Create a restricted runner group named `dsx-physical-apple` and scope it only to this trusted repository. Register separate dedicated Apple-silicon hosts with the custom labels `dsx-physical-macos-26-arm64` and `dsx-physical-macos-27-arm64`; a single host must not claim both OS-major labels. Create a protected environment named `dsx-physical-apple`, require operator approval, and prevent untrusted deployment branches.

Set the repository variable `DSX_CI_STATE_ROOT` to a clean absolute path owned by the dedicated runner account. The root and its pre-created `ledgers` and `quarantine` directories must be mode 0700, must not be symlinks, and must live on durable host-local storage rather than the checked-out workspace. The scripts reject an absent, permissive, foreign-owned, relative, or symlinked root.

The workflow has no pull-request trigger. Its destructive job is additionally guarded by GitHub's protected-ref context, the protected environment, an event allowlist, and the restricted runner group. Fork pull requests therefore cannot schedule either physical lane or receive a token. Manual dispatch is not a trust bypass: its selected ref must also be protected. Workflow permissions are limited to repository contents read and Actions run-state read. All third-party actions use immutable commit SHAs and checkout does not persist credentials.

The runner account needs the programs and runtime pair listed in the example config. Apple `container` CLI 1.2.2 and API server 1.2.2 must both attest, the service must be running, the builder must be observable, the host must be arm64, and the assigned OS major must match. The scripts do not install, start, stop, upgrade, or switch these host components.

## Per-run protocol

GitHub concurrency prevents ordinary overlap but is not the safety boundary. Each lane uses the same host-local atomic lock directory and records the repository, run, attempt, and OS-major owner. An existing lock is recoverable only when its exact ledger is valid, the GitHub run is terminal, the sweeper proves exact cleanup, and the ledger becomes clean. The lane then removes only that reconciled owner record and lock before acquiring a new lock. A live, non-terminal, malformed, foreign, or otherwise uncertain lock quarantines the host; it is never stolen.

While holding the lock, the lane performs the following ordered protocol:

1. Reject an existing quarantine marker and run the pre-job sweeper.
2. Capture canonical container, network, volume, builder, runtime, OS, and architecture JSON.
3. Require the exact macOS lane, arm64, healthy CLI/server 1.2.2 pair, and observable builder.
4. Generate `dsxci-<repository>-<run>-<attempt>-<random>` and durably write the baseline and intent ledger before Apple-runtime mutation.
5. Write a separate exact intent for each stopped, non-DSX-owned, deliberately DSX-like container/network/volume sentinel before creating it. Record each inspected sentinel digest after creation.
6. Run the read-mostly 1.2.2 canary, then the complete `tests/apple` suite with the fault-cleanup gate enabled.
7. In an unconditional workflow step, classify only resources absent from the baseline. Delete only a canonical resource with exactly the seven `dsx.ownership/v1` labels, or an exact runner sentinel named in the terminal run's ledger. Re-inspect identity and labels immediately before deletion.
8. Delete in dependency order: exact containers, then exact volumes, then exact networks. Compare the final resource set and builder status byte-canonically with the baseline and perform a second cleanup pass.
9. Write consolidated JSON evidence, mark the ledger clean, release the exact owned lock, and retain the evidence artifact for 30 days.

The cleanup path has no builder mutation and no general resource cleanup mode. It must never use a prune operation, a wildcard deletion, a delete-all operation, a runtime-system stop, uninstall, builder deletion, default-network deletion, or deletion based only on a DSX-looking name. Organic resources and pre-existing DSX resources are baseline state and remain untouched.

A normal test failure does not by itself quarantine a host if exact cleanup, repeated cleanup, sentinels, baseline, and builder comparison all succeed. An ownership, run-state, lock, sentinel, inventory, runtime, or cleanup uncertainty does quarantine it.

## Sweeper and interrupted runs

The pre-job sweeper exists for failures that shell traps cannot handle, including driver SIGKILL, runner loss, and power loss. It validates every non-clean ledger before considering mutation. It queries the GitHub Actions API only at the fixed GitHub API origin and requires the ledger's repository to equal the current trusted repository. Cleanup is allowed only when GitHub reports that exact run ID as `completed`.

For a terminal run, the sweeper compares the current inventory with the ledger baseline. Every new non-sentinel resource must have a canonical ID/name and exactly the complete DSX ownership tuple: managed, contract, project, sandbox, UUIDv7 run, kind, and role. The tuple's project, sandbox, kind, and role must reconstruct the observed canonical name. A runner sentinel must match its exact ledger name and recorded digest; an intent-only sentinel left by a crash may be removed only at that exact name and only when it has no DSX ownership label.

An unavailable or non-terminal GitHub run, malformed ledger, changed identity, incomplete/conflicting labels, unknown new resource, missing recorded sentinel, changed sentinel, builder drift, baseline drift, cleanup error, or live/uncertain lock writes `QUARANTINED.json` and blocks future jobs. The sweeper preserves the ledger and evidence. It never guesses, broadens ownership, or clears quarantine.

A boot service may invoke the same sweeper only after runner provisioning defines a non-secret, read-only Actions run-state credential and the same exclusive locking policy. That boot integration is not configured in this repository and is an external provisioning gate.

## Evidence contract

Each artifact is scoped by GitHub run, attempt, and OS major. It contains:

- host OS/architecture and Apple CLI/API-server/service attestation;
- canonical pre/post container, network, volume, default-network, and builder observations;
- baseline and ledger digests;
- unique run label and exact sentinel names/digests;
- recorded canary and suite argv, exit status, and postcondition without environment values;
- exact cleanup deletion records and repeated-clean result;
- `dsx-080-fault-cleanup.json` from the fault suite when that suite reached evidence emission;
- the consolidated `dsx-084-runner-evidence.json` when finalization completed; and
- sweeper API responses and recovery evidence when stale work was reconciled.

Do not add tokens, environment dumps, credentials, auth profiles, source bundles, host user paths beyond the configured state root, or unsanitized child output to evidence. A missing artifact, missing suite evidence, truncated/malformed JSON, failed upload, or absent consolidated evidence fails the release gate; it is not an acceptable partial success.

## Manual quarantine and recovery procedure

Quarantine recovery requires a human operator and an independent reviewer. It is an evidence-producing security operation, not routine cleanup.

1. Remove the host's custom runner label or runner-group eligibility out of band before investigation. Confirm in GitHub that it cannot accept another job.
2. Preserve the quarantine marker, lock owner, all ledgers, the run evidence directory, runner diagnostic logs, and the relevant GitHub run page. Hash the preserved files before any recovery mutation.
3. Establish exclusive physical/console control. Determine whether the recorded lock owner process still exists. A live or uncertain owner stops recovery; do not remove its lock.
4. Independently confirm in GitHub that the exact repository/run ID in the ledger is terminal. API failure, repository mismatch, or an indeterminate state leaves the host quarantined.
5. Capture fresh OS/runtime attestation, complete inventories, builder status, and exact sentinel inspections. Compare them with the ledger baseline. Classify every difference individually using the complete ownership and canonical-name rules above.
6. If any resource, label, manifest, sentinel, listener, temporary path, run state, or builder/default resource is ambiguous, preserve it and keep the host quarantined. Escalate for security review; never make the inventory “look clean” with broad deletion.
7. Only when every mutation is proven may the reviewed exact-cleanup path reconcile the terminal ledger. Capture each exact argv, exit, and postcondition, then repeat the clean comparison and prove the baseline, default network, sentinels, and builder are unchanged or correctly removed according to the ledger.
8. Produce recovery JSON containing operator and reviewer identities, timestamps, quarantine reason, GitHub terminal-state evidence, before/after hashes, every classification and exact deletion, repeated-clean result, sentinel/builder/default proof, and the decision. Store it with the original evidence.
9. The reviewer must approve the evidence before the operator removes only the reviewed stale lock and quarantine marker. Restore the one correct OS-major label and runner-group eligibility out of band. The next job must begin with a successful canary; it must not be treated as prior execution evidence.

If recovery cannot reach certainty, retire/reimage the dedicated runner under the infrastructure security process while preserving evidence. Reimaging is not proof that a failed DSX release candidate passed and cannot be used to admit a runtime version.

## External gates

Physical macOS 26 and macOS 27 host acquisition, runner registration, restricted-group configuration, environment protection, durable state-root creation, boot sweeper integration, and an observed quarantine/recovery drill remain external and incomplete. Release admission requires real evidence from both lanes on Apple `container` 1.2.2. Repository workflow/static checks can validate policy shape, but they cannot replace physical execution or establish compatibility.
