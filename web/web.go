// Package web embeds Castlet's server-rendered templates and static assets so
// the binary is self-contained. The server package parses Templates and serves
// Static; a deployment that wants a different frontend can supply its own
// server.Renderer instead.
package web

import "embed"

// Templates holds the html/template sources under templates/.
//
//go:embed templates
var Templates embed.FS

// Static holds CSS and JS under static/.
//
//go:embed static
var Static embed.FS
