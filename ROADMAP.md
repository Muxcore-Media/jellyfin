# Roadmap

## Done

- [x] Thin Jellyfin bridge (library refresh, webhook, play deep-link)
- [x] SettingsProvider for base URL / API key
- [x] Optional media tag + catalog entry
- [x] Production hardening (dial/subscribe retry, Health probe, webhook auth, durable settings, compose/Docker)
- [x] Playback session progress events (`playback.started` / `playback.progress` / `playback.stopped`)
- [x] Bidirectional library item sync (item links + MatchItem / SyncLibrary)

## Remaining

- [x] Admin-ui / media-ui surfaces for status, refresh, play links
- [x] Jellyfin ↔ MuxCore userdata handoff (progress/favorites; `USERDATA_SYNC`)

**Deferred (not open):** `contracts-playback` stays unshipped until a second playback backend (e.g. Plex) is committed — see workspace `MASTER-ROADMAP.md` §1.
