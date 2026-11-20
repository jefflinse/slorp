// Package opencodeplugin carries the OpenCode plugin shipped with Slorp.
package opencodeplugin

import "embed"

// Files contains the plugin's runtime files and package manifest.
//
//go:embed slorp.ts tui.ts presence.ts quotas.ts native.ts forms.ts service.js package.json
var Files embed.FS
