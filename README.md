# Jellyfin Playback Bridge

MuxCore playback module that talks to an external Jellyfin server.

Module ID: `jellyfin` · version `0.2.0` · `minCoreVersion` `0.4.0`

## Capabilities

- `playback.jellyfin` / `playback`
- `settings` — `base_url`, `api_key`, `webhook_secret`, `conflict_mode`, `sessions_poll_seconds`

## RPCs (`JellyfinBridge`)

- `RefreshLibrary` — POST `/Library/Refresh` (or `POST /Items/{id}/Refresh?Recursive=true` when `item_id` set)
- `PlayURL` — `{base}/web/index.html#!/details?id={item_id}` deep-link
- `Status` — configured flag, base URL, conflict mode, link count, sessions-poll flag
- `ListItemLinks` / `UpsertItemLink` / `DeleteItemLink` — durable MuxCore ↔ Jellyfin ID map
- `MatchItem` — match by provider IDs then path; upserts link when found
- `SyncLibrary` — pull/push reconcile (`direction`: `jellyfin` | `muxcore` | `both`)

## HTTP

- `POST /webhook` — Jellyfin webhook → event bus + typed playback events
- `GET /healthz`

When `webhook_secret` / `JELLYFIN_WEBHOOK_SECRET` is set, requests must send
`X-Jellyfin-Webhook-Secret: <secret>` or `Authorization: Bearer <secret>`.

## Playback events

| Jellyfin notification | MuxCore event |
|-----------------------|---------------|
| `PlaybackStart` | `playback.started` |
| `PlaybackProgress` | `playback.progress` |
| `PlaybackStop` | `playback.stopped` |

Payload JSON fields:

| Field | Notes |
|-------|-------|
| `item_id` / `jellyfin_item_id` | Jellyfin item GUID |
| `muxcore_id` | Resolved from item-link store when known |
| `user_id` / `user_name` | Jellyfin user |
| `session_id` | Session id when present |
| `position_ticks` / `duration_ticks` | Jellyfin ticks |
| `position_seconds` / `duration_seconds` | Derived (`ticks / 1e7`) |
| `media_path` / `title` | Optional |
| `is_paused` | Set by `/Sessions` poll |
| `notification_type` | Raw webhook type |

Raw webhook payloads are also published as `jellyfin.<notificationtype>` (lowercased),
or `jellyfin.library.changed` when the type is missing.

Optional: set `sessions_poll_seconds` > 0 to poll `/Sessions` when webhooks are incomplete.

## Library sync

Durable links live in `{JELLYFIN_DATA_DIR}/settings.json`.

Match order: existing link with Jellyfin ID → provider IDs → filesystem path.

Conflict mode (`conflict_mode`):

| Mode | Behavior |
|------|----------|
| `jellyfin` (default) | Jellyfin path/title/provider IDs win on conflict |
| `muxcore` | Keep MuxCore metadata when both sides differ |
| `manual` | Do not overwrite non-empty MuxCore fields from Jellyfin |

On `download.completed` / `media.imported` / `media.file.imported`, triggers a full library refresh
and attempts to match the imported item into the link store.

## Env

| Var | Default | Notes |
|-----|---------|-------|
| `JELLYFIN_BASE_URL` | | Jellyfin server URL |
| `JELLYFIN_API_KEY` | | API key |
| `JELLYFIN_WEBHOOK_SECRET` | | Shared secret for `/webhook` |
| `JELLYFIN_GRPC_ADDR` | `:9475` | gRPC listen |
| `JELLYFIN_HTTP_ADDR` | `:8475` | HTTP listen (webhook / healthz) |
| `JELLYFIN_DATA_DIR` | `/var/lib/muxcore-jellyfin` | Durable settings + links |
| `JELLYFIN_CONFLICT_MODE` | `jellyfin` | Sync conflict policy |
| `JELLYFIN_SESSIONS_POLL_SECONDS` | `0` | `/Sessions` poll; `0` = off |
| `MUXCORE_GRPC_ADDR` | | Core mesh (webhook publish + import refresh) |
| `MUXCORE_INSECURE_DISABLE_TLS` | `false` | Disable TLS to core (dev) |

## Health

Module `Health` probes Jellyfin `GET /System/Info` when configured.
Docker `HEALTHCHECK` runs `/module --health-check` against local `/healthz`.
