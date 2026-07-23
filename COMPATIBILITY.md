# Compatibility

## Core Version

| Module Version | Core Version | Status |
|----------------|-------------|--------|
| v0.2.0         | v0.4.0+     | Current |
| v0.1.0         | v0.4.0+     | Superseded |

## Contracts

This module intentionally uses a **module-local protobuf** (`proto/jellyfinv1`) rather than a
shared `contracts-*` package. Playback deep-links, library refresh, and item-link sync are
Jellyfin-specific; other playback backends are not expected to share the same RPC surface yet.

| Surface | Location | Status |
|---------|----------|--------|
| `JellyfinBridge` gRPC | `proto/jellyfinv1` | Current |
| Shared playback contract | — | Not planned until a second backend needs it |

Capabilities advertised: `playback.jellyfin`, `playback`, `settings`.

## Breaking Changes

This is a pre-1.0 module. Interfaces may change without notice.
