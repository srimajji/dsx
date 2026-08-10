# DSX

DSX is a daemonless Go CLI for running isolated Linux development sandboxes on Apple-silicon Macs with Apple’s [`container`](https://github.com/apple/container) runtime.

It supports live project workspaces, independent named Git clones, coding-agent harnesses, project services, loopback port publication, isolated browser testing, persistent authentication profiles, and ownership-safe cleanup.

> The MVP implementation is present, but DSX is not yet a signed or notarized public release. See the documented external evidence and release gates before treating it as production-ready.

## Documentation

- [Getting started](docs/manual/getting-started.md)
- [Complete user and operator manual](docs/manual/user-guide.md)
- [Product requirements](docs/PRD.md)
- [Implementation architecture](docs/adr/0001-dsx-implementation-architecture.md)
- [MVP implementation outcome](docs/implementation-plan.md)
- [Post-MVP backlog](docs/post-mvp-backlog.md)

## Build

DSX requires Go 1.26.5. Build the Darwin/arm64 host CLI and Linux/arm64 guest helper together:

```console
make build
```

Then follow the [user manual](docs/manual/user-guide.md) for Apple runtime prerequisites, project configuration, approval, sandbox workflows, authentication, browser isolation, and cleanup.
