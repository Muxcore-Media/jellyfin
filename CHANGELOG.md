# Changelog


## [0.2.4] — 2026-08-10

### Fixed
- Sync moduleVersion / muxcore.json to **0.2.4**.

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.2.0] — 2026-07-22

### Added

- Webhook shared-secret auth (`webhook_secret` / `JELLYFIN_WEBHOOK_SECRET`)
- Durable settings + item-link store under `JELLYFIN_DATA_DIR`
- Typed playback events: `playback.started`, `playback.progress`, `playback.stopped`
- Optional `/Sessions` polling (`sessions_poll_seconds`)
- Library sync RPCs: `ListItemLinks`, `UpsertItemLink`, `DeleteItemLink`, `MatchItem`, `SyncLibrary`
- Conflict modes: `jellyfin` (default), `muxcore`, `manual`
- Docker `--health-check` flag hitting local `/healthz`

### Fixed

- Core dial/subscribe race — dial with retry, then subscribe
- `Health` probes Jellyfin `/System/Info` when configured
- Compose publishes `:8475` and passes `JELLYFIN_*` env vars
- Scaffold leftovers (`yourorg` / `YOUR_MODULE_*`) cleaned up

## [0.1.0] — 2026-07-21

### Added

- Thin Jellyfin bridge: `RefreshLibrary`, `PlayURL`, `Status`
- Settings for `base_url` / `api_key`
- HTTP webhook → event bus; `healthz`
- Refresh on `download.completed` / `media.imported`
