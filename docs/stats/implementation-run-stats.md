# DSX MVP implementation-run statistics

- **Report date:** 2026-08-10
- **Primary session:** `019fe829-9e3f-7000-8e74-c41cad2a10e8`
- **Model:** `openai-codex/gpt-5.6-sol`
- **Telemetry sources:** `omp stats --json`, the primary session JSONL, its 147 child-agent JSONLs, and the `history://` agent roster

## Scope

Primary scope: the original DSX MVP implementation session.

- Main session: `019fe829-9e3f-7000-8e74-c41cad2a10e8`
- Started: **2026-08-09 20:14:29 UTC**
- Last recorded event: **2026-08-10 18:57:37 UTC**
- Wall-clock span: **22h 43m**
- Includes the main coordinator and all 147 child-agent session logs.
- Excludes unrelated OMP projects and separate top-level planning/closeout sessions.

## Executive summary

| Metric | Result |
|---|---:|
| Model requests | 15,145 |
| Successful requests | 15,116 |
| Model errors | 29 — 0.19% |
| Aborted responses | 13 |
| Uncached input tokens | 41,441,131 |
| Cached input tokens read | 1,663,357,568 |
| Generated output tokens | 4,186,198 |
| Reasoning tokens | 1,951,697 |
| Total accounted tokens | 1,708,984,897 |
| Recorded cache rate | 97.57% |
| Estimated recorded cost | $1,164.47 |
| Agent sessions | 148 |
| Subagent sessions | 147 |
| Peak overlapping agent sessions | 31 |
| Tool calls | 14,996 |

### Token accounting

```text
1,663,357,568 cached input
   41,441,131 uncached input
    4,186,198 generated output
────────────────────────────
1,708,984,897 accounted tokens
```

“Generated tokens” means the **4.19 million output tokens**. Input and cache tokens were processed or reused, not generated.

The 1,951,697 reasoning tokens are a subset of output tokens, so they must not be added again. Approximately 2,234,501 output tokens were non-reasoning text, tool-call arguments, and structured results.

## Model

Every recorded request used one model:

| Provider | Model | Requests |
|---|---|---:|
| OpenAI Codex | `openai-codex/gpt-5.6-sol` | 15,145 |

No model switching was recorded within this implementation cohort.

## Agent composition

There were **148 agent sessions**: one main coordinator plus 147 subagents.

| Agent role | Sessions | Requests | Input tokens | Output tokens | Tool calls | Estimated cost |
|---|---:|---:|---:|---:|---:|---:|
| Main coordinator | 1 | 4,211 | 11,739,831 | 884,555 | 4,197 | $413.69 |
| Implementation/remediation task | 101 | 7,466 | 18,558,897 | 2,435,431 | 7,386 | $484.14 |
| Reviewer | 21 | 1,701 | 4,966,585 | 355,141 | 1,658 | $124.54 |
| Security reviewer | 21 | 1,653 | 5,753,999 | 488,360 | 1,641 | $135.74 |
| Scout | 4 | 114 | 421,819 | 22,711 | 114 | $6.36 |
| **Total** | **148** | **15,145** | **41,441,131** | **4,186,198** | **14,996** | **$1,164.47** |

Subagents generated **78.87%** of all output tokens. The main coordinator generated 21.13%.

### Scout agents

The four read-only research agents were:

- `CoreSourceResearch`
- `TestsQAResearch`
- `BuildConfigResearch`
- `ScriptsDocsResearch`

### Implementation and remediation agents

The 101 task-agent sessions covered these areas:

- Foundation, configuration, planning, inspection, approval, TUI, and terminal safety
- Apple lifecycle, manifests, ownership, ports, PTY, and cleanup
- Guest protocol, supervisor, services, health, and logs
- OMP, Codex, Claude, OpenCode, authentication, MCP, and login
- Named clones, Git bundles, status, diff, fetch, apply, and data-loss guards
- Browser images, runtime isolation, OAuth, Leapp, and private relays
- Fault cleanup, performance, fuzz/race coverage, packaging, CI, and documentation
- Multiple targeted remediation rounds following reviewer findings

Representative named agents included:

- `ConfigSchema`, `ProjectInspectors`, `PlanResolution`, `PlanApproval`
- `ManifestLocks`, `AppleLifecycleAdapter`, `GuestSupervisor`
- `HarnessOMP`, `HarnessCodex`, `HarnessClaude`, `HarnessOpenCode`
- `CloneLifecycle`, `GitTransfer`, `CloneGitCLI`, `UnfetchedGuard`
- `BrowserOrchestration`, `OAuthCallbackBridge`, `LeappBridge`, `PrivateTCPRelay`
- `FaultCleanupSuite`, `PerformanceBudgets`, `PackagingRelease`, `SelfHostedCI`
- `FinalCloneLifecycleFix`, `FinalHelperLifetimeFix`, `AppleMountAuthorityFix`

### Review agents

There were 21 general-review sessions spanning:

- Slices 1–8
- Re-review and final-review passes
- Composite-service gate review
- Integrated implementation review
- Final remediation review

There were another 21 security-review sessions covering:

- Runtime and ownership boundaries
- Terminal and PTY behavior
- Credentials and authentication
- Clone/Git result safety
- Browser and network isolation
- Leapp and private relays
- Cleanup and release hardening
- Final integrated security sweeps

At report time:

- **93** subagents remained registered and parked.
- **54** older subagents were persisted on disk.
- No subagent was still actively executing.

## Tool-call report

Assistant tool calls and recorded tool-execution starts both counted exactly **14,996**.

| Tool | Calls | Share |
|---|---:|---:|
| `read` | 6,018 | 40.13% |
| `grep` | 2,297 | 15.32% |
| `edit` | 2,085 | 13.90% |
| `hub` | 1,713 | 11.42% |
| `bash` | 1,395 | 9.30% |
| `yield` | 579 | 3.86% |
| `write` | 385 | 2.57% |
| `glob` | 300 | 2.00% |
| `todo` | 90 | 0.60% |
| `task` | 58 | 0.39% |
| `eval` | 46 | 0.31% |
| `web_search` | 16 | 0.11% |
| Node REPL MCP | 6 | 0.04% |
| GitHub code-search MCP | 4 | 0.03% |
| `ask` | 1 | <0.01% |
| Other GitHub MCP calls | 3 | 0.02% |
| **Total** | **14,996** | **100%** |

The 58 `task` calls spawned 147 child sessions because independent agents were dispatched in batches—an average of **2.53 agents per dispatch**.

### Tool-use interpretation

- Research and repository lookup—`read`, `grep`, `glob`, and web search—accounted for roughly **57.6%** of calls.
- Direct file mutation—`edit` and `write`—accounted for **16.5%**.
- Agent coordination—`task`, `hub`, and `yield`—accounted for **15.7%**.
- Shell and programmatic evaluation accounted for approximately **9.6%**.

This was therefore primarily a repository-analysis and verification workload, not a shell-heavy generation run.

## Request performance

| Metric | Average |
|---|---:|
| Uncached input per request | 2,736 tokens |
| Generated output per request | 276 tokens |
| Model response duration | 9.62 seconds |
| Time to first token | 3.42 seconds |

The peak was **31 overlapping agent-session lifetimes**, one below the configured concurrency cap of 32.

## Highest-output sessions

Excluding the main coordinator:

| Agent | Role | Output tokens |
|---|---|---:|
| `GuestSupervisor` | Task | 76,615 |
| `GuestBoundaryRemediation` | Task | 67,591 |
| `AppleLifecycleAdapter` | Task | 58,828 |
| `LeappMirrorLease` | Task | 57,713 |
| `HarnessAuthRemediation` | Task | 51,287 |
| `SelfHostedCI` | Task | 48,325 |
| `Slice4Resecurity` | Security reviewer | 46,704 |
| `PackagingRelease` | Task | 46,061 |
| `ResumeCloneLifecycle` | Task | 45,774 |
| `PersistentHostRelayLease` | Task | 45,352 |

The main coordinator itself generated 884,555 output tokens across 4,211 model requests.

## Broader repository-folder totals

`omp stats --json` also reported totals for **all** OMP sessions under `/Volumes/Dev/work/sandbox-dx`, including other planning, closeout, and reporting sessions:

| Metric | Implementation cohort | Entire `sandbox-dx` folder |
|---|---:|---:|
| Requests | 15,145 | 15,495 |
| Input tokens | 41,441,131 | 42,851,805 |
| Output tokens | 4,186,198 | 4,332,291 |
| Cache-read tokens | 1,663,357,568 | 1,691,069,312 |
| Estimated cost | $1,164.47 | $1,189.76 |

The implementation cohort accounts for approximately:

- **97.7%** of folder requests
- **96.6%** of folder output
- **97.9%** of recorded cost

## Caveats

- Agent counts represent **agent sessions**, not unique humans or unique logical tasks. Resume and remediation agents count separately.
- Cached tokens represent reused model context. They materially increase accounted token volume without representing newly supplied prompts.
- Reasoning tokens are included in output tokens.
- Cost is OMP’s recorded estimate, not necessarily the final provider invoice or subscription charge.
- The folder-wide snapshot changes as this report and future sessions continue. The implementation cohort is anchored to the recorded session files through 2026-08-10 18:57:37 UTC.
- Primary telemetry path: `/Users/dragon/.omp/agent/sessions/--Volumes-Dev-work-sandbox-dx--/2026-08-09T20-14-29-439Z_019fe829-9e3f-7000-8e74-c41cad2a10e8.jsonl` and its companion directory.
