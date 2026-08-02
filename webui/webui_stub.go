//go:build nowebui

// Package webui's stub build: compiled when the binary is built with
// -tags nowebui, producing a pure-API server with no templates, no static
// assets, and no html/template dependency. The Register functions are never
// called because config validation rejects the webui/admin-page groups when
// Available is false.
package webui

import "github.com/go-chi/chi/v5"

// Available is false in -tags nowebui builds; see webui.go for the real build.
const Available = false

// LibraryPath mirrors the real build's constant so non-UI code can name the
// library page's URL without a build tag.
const LibraryPath = "/library"

// Register is a no-op in nowebui builds.
func Register(r chi.Router, apiBase, gitRepo string, federated bool) {}

// RegisterAdminPage is a no-op in nowebui builds.
func RegisterAdminPage(r chi.Router, apiBase, gitRepo string) {}
