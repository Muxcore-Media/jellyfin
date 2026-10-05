# Changelog

## [0.3.0] - 2026-10-05

### Changed
- CI runs on GitHub-hosted runners from the umbrella template; retired-origin workflows removed.
- Dependencies resolve from published GitHub tags (no filesystem `replace`); requires core v0.6.0.

## [0.3.0] — 2026-08-20

### Added

- **Userdata handoff**: sync Jellyfin UserData (progress / watched / favorites) into MuxCore
  `userdata-local` (or publish `userdata.jellyfin.synced` when no URL is set)
- Optional MuxCore → Jellyfin push (`USERDATA_PUSH_TO_JELLYFIN=1`, `POST /userdata/from-muxcore`)
- Periodic pull when `USERDATA_SYNC=1` (`USERDATA_SYNC_INTERVAL_SECONDS`, default 300)
- Live mirror of webhook/session playback events into MuxCore userdata while sync is enabled
- HTTP: `GET|POST /userdata/sync`, `POST|PUT /userdata/from-muxcore`, `GET /userdata/status`
- Env aliases: `JELLYFIN_URL` (= `JELLYFIN_BASE_URL`), `USERDATA_LOCAL_URL`, `USERDATA_USER_MAP`
- Capability `userdata.sync`

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
- Refresh on `download.completed` / `media.file.imported`
