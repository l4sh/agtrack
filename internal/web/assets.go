// Package web embeds the spec viewer's static assets for the HTTP server.
package web

import "embed"

//go:embed assets
var Assets embed.FS
