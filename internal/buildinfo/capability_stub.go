//go:build !webui

package buildinfo

// HasWebUI is false in a default build: the web assets are embedded only under
// the webui tag, so `aiusage serve` has nothing to serve. See capability_webui.go.
const HasWebUI = false
