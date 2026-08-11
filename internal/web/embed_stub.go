//go:build !webui

package web

import "net/http"

// hasEmbeddedUI is false in a default build: the UI assets are embedded only
// under the webui tag (embed_webui.go), which the release pipeline sets after
// building webui/. A default `go build ./...` therefore needs no Node toolchain
// and no dist directory, and this file is why - it is the stub that keeps the
// package compiling without them.
const hasEmbeddedUI = false

// uiHandler serves the honest 404 of a build with no page in it. The API is
// fully functional; only the SPA is missing, which is exactly what it says.
func uiHandler() http.Handler { return stubUIHandler() }
