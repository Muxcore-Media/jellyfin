# AGENTS.md — jellyfin

MuxCore sidecar module (`jellyfin`). Workspace deploy and SSH: [`../AGENTS.md`](../AGENTS.md). Default ports: [`_mvp/PORTS.md`](../_mvp/PORTS.md).

## Module identity

| Field | Value |
|-------|-------|
| Directory | `jellyfin` |
| Capabilities | see muxcore.json |
| Contracts | none declared |

## Agent rules

- Modules run as gRPC sidecars; capabilities are the security boundary.
- TLS required in production (`MUXCORE_INSECURE_DISABLE_TLS` is dev-only).
- Match existing Go patterns; run `gofmt` and package tests before finishing.
- Cross-module events: prefer `github.com/Muxcore-Media/contracts-media/events` over deprecated `core/pkg/contracts` aliases.
- Do not edit polluted workspace dumps (see `MASTER-ROADMAP.md` Appendix H).
- Erasure (ADR-0035): only the identity provider's ledger, via `erasure.Reconciler` (`internal/erasure.go`), may erase `userdata_user_map` entries; never add an event/HTTP trigger and never call Jellyfin to delete users.

## Build

```bash
cd jellyfin
go test ./...
```
