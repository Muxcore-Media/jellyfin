// Package manifest embeds the module's muxcore.json so that the version the
// module reports is read from that single source (ADR-0021).
package manifest

import _ "embed"

// ManifestJSON is the embedded muxcore.json. The module's reported version is
// derived from it via modulesdk.ManifestVersion; there is no other source of
// truth for the version (ADR-0021).
//
//go:embed muxcore.json
var ManifestJSON []byte
