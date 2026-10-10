# Jellyfin Playback Bridge

MuxCore playback module that talks to an external Jellyfin server.

Module ID: `jellyfin` · version `0.3.0` · `minCoreVersion` `0.4.0`

## Product decision: bridge now, native player end state

**Do not confuse these:**

| Horizon | Playback UI |
|---------|-------------|
| **End state** | `media-ui-app` **replaces** Jellyfin web (OSD, tracks, resume, transcoder) — see workspace `MASTER-ROADMAP.md` |
| **Near-term** | Households may play via this **Jellyfin bridge** (deep link / JF clients) while MuxCore owns browse, request, automation, and userdata sync |

Native `<video>` / `VideoPlayer` in `media-ui-app` is **on the path to replacement**, not a permanent “dev only” dead end. Keep the bridge for library sync and optional JF clients even after native play ships.

### Userdata / progress handoff

| Surface | Source of truth while playing | After sync |
|---------|------------------------------|------------|
| MuxCore continue-watching / favorites / prefs / queue | BFF `/api/userdata` → `userdata-local` | Reflects Jellyfin UserData when `USERDATA_SYNC=1` |
| Jellyfin playback position / watched / favorites | Jellyfin UserData API | Optional push from companion via `USERDATA_PUSH_TO_JELLYFIN=1` |
| ID map MuxCore ↔ Jellyfin | `jellyfin` module item-link store | Progress keys prefer `muxcore_id` when linked |

**Handoff flow**

1. Browse/request in MuxCore UI; progress cached locally and synced to server userdata.
2. Play via `PlayURL` / deep-link into Jellyfin web (or native JF clients).
3. Jellyfin webhooks (or sessions poll) publish playback events **and** mirror progress into MuxCore userdata when sync is enabled.
4. Periodic pull (`USERDATA_SYNC=1`) copies JF resumable / played / favorite items into `userdata-local` (or publishes `userdata.jellyfin.synced` if no URL).
5. Companion UI PUTs to `/api/userdata`; BFF may notify the bridge (`JELLYFIN_USERDATA_PUSH_URL`) so JF UserData stays aligned.

Do **not** invent a second playback backend or revive `contracts-playback` until a second server (e.g. Plex) is committed.

## Capabilities

- `playback.jellyfin` / `playback` / `userdata.sync`
- `settings` — connection, library conflict, sessions poll, userdata sync knobs

## RPCs (`JellyfinBridge`)

- `RefreshLibrary` — POST `/Library/Refresh` (or `POST /Items/{id}/Refresh?Recursive=true` when `item_id` set)
- `PlayURL` — `{base}/web/#/details?id={item_id}` deep-link (Jellyfin 10.9+; set `play_url_style=legacy` for 10.8 `#!/details`)
- `Status` — configured flag, base URL, conflict mode, link count, sessions-poll flag, userdata sync, SSE connected
- `ListSessions` — live Jellyfin `/Sessions` snapshot (id, user, item, position, paused, device)
- `ListItemLinks` / `UpsertItemLink` / `DeleteItemLink` — durable MuxCore ↔ Jellyfin ID map
- `MatchItem` — match by provider IDs then path; upserts link when found
- `SyncLibrary` — pull/push reconcile (`direction`: `jellyfin` | `muxcore` | `both`)

## HTTP

- `POST /webhook` — Jellyfin webhook → event bus + typed playback events (+ userdata mirror when sync on)
- `GET|POST /userdata/sync` — pull Jellyfin UserData → MuxCore userdata now
- `POST|PUT /userdata/from-muxcore?user_id=` — push companion blob into Jellyfin (requires push enabled)
- `GET /userdata/status` — sync flags

All userdata routes and `/webhook` require `JELLYFIN_WEBHOOK_SECRET` (or admin-ui `webhook_secret`).
When the secret is unset, requests are rejected with 401.
Send `X-Jellyfin-Webhook-Secret: <secret>` or `Authorization: Bearer <secret>`.
- `GET /healthz`

When `webhook_secret` / `JELLYFIN_WEBHOOK_SECRET` is set, requests must send
`X-Jellyfin-Webhook-Secret: <secret>` or `Authorization: Bearer <secret>`.
The secret is **required** — empty secret rejects all webhook/userdata HTTP calls.

Optional plugin SSE (`JELLYFIN_SSE=1`): connects to Jellyfin `/api/sse/events` for live playback.
Stock Jellyfin returns 404 without the plugin; default is **off** so soak hosts do not reconnect forever.

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

On `download.completed` / `media.file.imported`, triggers a full library refresh
and attempts to match the imported item into the link store.

## Userdata sync

When `USERDATA_SYNC=1`:

1. Lists Jellyfin `/Users`, then for each user pulls `/Users/{id}/Items` with filters
   `IsResumable`, `IsPlayed`, and `IsFavorite` (fields include `UserData`).
2. Maps item IDs through the item-link store (`muxcore_id` preferred; else `jf:{id}`).
3. Writes a mergeable progress/favorites blob to `USERDATA_LOCAL_URL` (`PUT /userdata?user_id=`),
   or publishes event `userdata.jellyfin.synced` when the URL is empty.
4. Also mirrors live webhook/session playback into userdata between polls.

MuxCore → Jellyfin (optional): set `USERDATA_PUSH_TO_JELLYFIN=1` and point the BFF at
`JELLYFIN_USERDATA_PUSH_URL=http://jellyfin:8475/userdata/from-muxcore`.

User mapping: Jellyfin `Name` becomes MuxCore `user_id` by default. Override with
`USERDATA_USER_MAP=jfUserId:muxUser,OtherName:bob`.

## Env

| Var | Default | Notes |
|-----|---------|-------|
| `JELLYFIN_BASE_URL` / `JELLYFIN_URL` | | Jellyfin server URL (`JELLYFIN_URL` is an alias) |
| `JELLYFIN_API_KEY` | | API key |
| `JELLYFIN_WEBHOOK_SECRET` | | Shared secret for `/webhook` |
| `JELLYFIN_GRPC_ADDR` | `:9475` | gRPC listen |
| `JELLYFIN_HTTP_ADDR` | `:8475` | HTTP listen (webhook / userdata / healthz) |
| `JELLYFIN_DATA_DIR` | `/var/lib/muxcore-jellyfin` | Durable settings + links |
| `JELLYFIN_CONFLICT_MODE` | `jellyfin` | Sync conflict policy |
| `JELLYFIN_SESSIONS_POLL_SECONDS` | `0` | `/Sessions` poll; `0` = off |
| `JELLYFIN_SSE` | `0` | `1` enables plugin SSE at `/api/sse/events` (requires Jellyfin plugin) |
| `JELLYFIN_PLAY_URL_STYLE` | `modern` | `modern` (10.9+ `/web/#/details`) or `legacy` (10.8 `#!/details`) |
| `USERDATA_SYNC` | `0` | `1` enables JF→MuxCore userdata handoff |
| `USERDATA_SYNC_INTERVAL_SECONDS` | `300` | Pull interval (`USERDATA_SYNC_INTERVAL` alias) |
| `USERDATA_LOCAL_URL` | | e.g. `http://userdata-local:9680` |
| `USERDATA_PUSH_TO_JELLYFIN` | `0` | `1` enables companion → JF UserData |
| `USERDATA_USER_MAP` | | `jfId:muxUser,Name:other` comma map |
| `MUXCORE_GRPC_ADDR` | | Core mesh (webhook publish + import refresh) |
| `MUXCORE_INSECURE_DISABLE_TLS` | `false` | Disable TLS to core (dev) |

### mediauiprox (BFF) knobs

| Var | Default | Notes |
|-----|---------|-------|
| `USERDATA_LOCAL_URL` | | Prefer mesh `userdata-local` for `/api/userdata` |
| `USERDATA_PREFER_MESH` | `1` | Set `0` to force BFF-local files even when URL is set |
| `JELLYFIN_USERDATA_PUSH_URL` | | e.g. `http://jellyfin:8475/userdata/from-muxcore` |

## User erasure (ADR-0035)

The bridge runs the shared `erasure.Reconciler` (core SDK `sdk/go/module/erasure`). The identity
provider's erasure ledger is the only authority; the bridge discovers the exclusive `identity`
provider through core, verifies its certificate CN, applies each tombstone it has not applied, and
acknowledges. It sweeps at startup and every `ERASURE_SWEEP_INTERVAL` (default `5m`; invalid values
fail startup). It needs a core connection (`MUXCORE_GRPC_ADDR`): the household and staging profiles
refuse to start without one, dev logs a warning and runs without it.

Disposition: `userdata_user_map` entries whose **value** is the erased user id are deleted, in one
`settings.json` replace that also records the erasure id under `erasure_applied`. Re-applying an
erasure id is a no-op. Retained, by design: Jellyfin server accounts and their data (the bridge
calls no Jellyfin API to delete users), and map entries in a different id space (Jellyfin ids or
names, other users' ids, username values). An erased id is not re-seeded into the map from
`USERDATA_USER_MAP` or the settings API, and bridge writes for it are refused.

## Health

Module `Health` probes Jellyfin `GET /System/Info` when configured.
Docker `HEALTHCHECK` runs `/module --health-check` against local `/healthz`.
