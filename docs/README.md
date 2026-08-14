# DSX documentation

## Normative contracts

- [PRD.md](./PRD.md) — product requirements (authoritative)
- [adr/](./adr/) — architecture decision records (authoritative)
- [post-mvp-backlog.md](./post-mvp-backlog.md) — MVP closeout, gaps, and living backlog

## Folders

| Folder | Contents | Naming |
|---|---|---|
| [adr/](./adr/) | Accepted architecture decisions | `NNNN-title.md` |
| [research/](./research/) | Dated research notes and comparisons; inform decisions but are not contracts | `research-YYYY-MM-DD-topic.md` |
| [reviews/](./reviews/) | Dated pressure tests and hardening reviews | `review-YYYY-MM-DD-topic.md` |
| [plans/](./plans/) | Implementation and migration plans | `topic-plan.md` |
| [operations/](./operations/) | Operator and runner procedures | — |
| [manual/](./manual/) | End-user documentation | — |
| [stats/](./stats/) | Implementation run statistics | — |

Precedence when documents disagree: PRD and ADRs govern; plans execute them; research and reviews inform them.
