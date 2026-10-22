// Package web holds the embedded frontend: html/template pages and the static
// assets (drift stylesheet, fonts, page script).
package web

import "embed"

// Static is the static asset tree, served under /static/.
//
//go:embed static
var Static embed.FS

// Templates holds the page templates.
//
//go:embed templates
var Templates embed.FS
