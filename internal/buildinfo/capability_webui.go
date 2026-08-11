//go:build webui

package buildinfo

// HasWebUI is the compile-time answer to "does this binary carry the web UI".
// It is true here because the webui build tag is set, which is the same tag
// internal/web's //go:embed of the built assets is guarded by: one tag, two
// files, and TestBuildTagsAgreeOnTheEmbeddedUI (internal/cmd) fails the build
// if they ever disagree.
const HasWebUI = true
