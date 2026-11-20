// Package opencodeplugin carries the OpenCode plugin shipped with Slorp.
package opencodeplugin

import "embed"

// Files contains the plugin's runtime files and package manifest.
//
//go:embed slorp.ts tui.ts presence.ts quotas.ts package.json
var Files embed.FS
