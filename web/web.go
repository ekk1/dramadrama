// Package web embeds the HTML templates and static assets of the workbench.
package web

import "embed"

// FS holds templates/*.html and static/*.
//
//go:embed templates static
var FS embed.FS
