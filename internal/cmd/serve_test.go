package cmd

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RandomCodeSpace/aiusage/internal/buildinfo"
	"github.com/RandomCodeSpace/aiusage/internal/config"
	"github.com/RandomCodeSpace/aiusage/internal/store"
	"github.com/RandomCodeSpace/aiusage/internal/web"
)

// TestBuildTagsAgreeOnTheEmbeddedUI: the webui tag is asserted in two packages -
// buildinfo folds it into the build identity, internal/web embeds the assets. If
// they ever disagree, a binary would advertise a capability it does not have (or
// hide one it does), and every version-sync decision downstream would be wrong.
// This is the only place that sees both.
func TestBuildTagsAgreeOnTheEmbeddedUI(t *testing.T) {
	if buildinfo.HasWebUI != web.HasEmbeddedUI() {
		t.Fatalf("buildinfo.HasWebUI = %v but web.HasEmbeddedUI() = %v; the two webui build tags have drifted",
			buildinfo.HasWebUI, web.HasEmbeddedUI())
	}
	if strings.HasSuffix(buildinfo.Identity(), "+webui") != web.HasEmbeddedUI() {
		t.Fatalf("Identity() = %q does not match the embedded-UI capability %v",
			buildinfo.Identity(), web.HasEmbeddedUI())
	}
}

// TestServeIsNotADaemonSpawner: starting a read-only dashboard must never have
// the side effect of spawning a background writer.
func TestServeIsNotADaemonSpawner(t *testing.T) {
	if !daemonSkip["serve"] {
		t.Fatal("serve is missing from daemonSkip; asking for a web page would spawn a collection daemon")
	}
}

// TestServeWithoutEmbeddedUIRefuses is the untagged half of issue #61: no page
// in the binary means serve exits with an error, after printing guidance that
// says how to get one.
func TestServeWithoutEmbeddedUIRefuses(t *testing.T) {
	if web.HasEmbeddedUI() {
		t.Skip("this build embeds the UI; the refusal path is the untagged one")
	}

	out, err := runCmd(t, "serve")
	if err == nil {
		t.Fatal("serve returned no error in a build with no embedded UI")
	}
	if !errors.Is(err, errNoEmbeddedUI) {
		t.Fatalf("serve error = %v, want errNoEmbeddedUI", err)
	}
	for _, want := range []string{"webui", "npm run build", "-tags webui"} {
		if !strings.Contains(out, want) {
			t.Errorf("guidance is missing %q:\n%s", want, out)
		}
	}
}

// TestNoEmbeddedUIErrorIsAProperErrorString: the guidance is a package-level
// const, not the error value. An error string stays lowercase, one line, no
// trailing punctuation (staticcheck ST1005), because it may be wrapped into a
// longer sentence by any caller.
func TestNoEmbeddedUIErrorIsAProperErrorString(t *testing.T) {
	msg := errNoEmbeddedUI.Error()
	if strings.ContainsAny(msg, "\n\t") {
		t.Errorf("error string %q spans lines; that is what the guidance const is for", msg)
	}
	if strings.HasSuffix(msg, ".") || strings.HasSuffix(msg, "!") {
		t.Errorf("error string %q ends in punctuation", msg)
	}
	if msg != strings.ToLower(msg) {
		t.Errorf("error string %q is not lowercase", msg)
	}
	if !strings.Contains(noEmbeddedUIGuidance, "\n") {
		t.Error("the guidance const is a single line; it is meant to be the multi-line explanation")
	}
}

// TestDaemonWarnsWithoutTheUIAndKeepsCollecting: the collector does not need the
// page and must never stop over it. The notice explains why serve is gone; it is
// not a refusal.
func TestDaemonWarnsWithoutTheUIAndKeepsCollecting(t *testing.T) {
	if !strings.Contains(noEmbeddedUIDaemonNotice, "collection is unaffected") {
		t.Errorf("daemon notice %q does not say collection continues", noEmbeddedUIDaemonNotice)
	}
	if strings.Contains(noEmbeddedUIDaemonNotice, "\n") {
		t.Error("the daemon notice spans lines; one line in a log is the whole point")
	}
}

// TestDoctorReportsTheWebUICapability: doctor is where a user finds out which
// kind of build they installed.
func TestDoctorReportsTheWebUICapability(t *testing.T) {
	out, err := runCmd(t, "doctor", "--config", offlineConfig(t), "--home", t.TempDir())
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if !strings.Contains(out, "web ui:") {
		t.Fatalf("doctor printed no web ui line:\n%s", out)
	}
	want := "not embedded"
	if web.HasEmbeddedUI() {
		want = "embedded ("
	}
	if !strings.Contains(out, want) {
		t.Errorf("doctor web ui line does not say %q:\n%s", want, out)
	}
	if !strings.Contains(out, "build:    "+buildinfo.Identity()) {
		t.Errorf("doctor does not print the build identity %q:\n%s", buildinfo.Identity(), out)
	}
}

// TestIsLoopbackAddr guards the one flag on this command that can publish the
// ledger to the network.
func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:37800", true},
		{"localhost:37800", true},
		{"[::1]:37800", true},
		{"0.0.0.0:37800", false},
		{":37800", false},
		{"192.168.1.10:37800", false},
		{"aiusage.example:443", false},
	}
	for _, tc := range tests {
		if got := isLoopbackAddr(tc.addr); got != tc.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

// TestDaemonProbeReportsAStoppedDaemon: with no daemon holding the lock, the
// probe says so instead of inventing a pid.
func TestDaemonProbeReportsAStoppedDaemon(t *testing.T) {
	cfg := config.Config{
		PIDPath:         filepath.Join(t.TempDir(), "aiusage.pid"),
		IntervalSeconds: 300,
	}
	info := daemonProbe(cfg)()
	if info.Running || info.PID != 0 || info.Uptime != 0 {
		t.Errorf("probe = %+v, want a stopped daemon", info)
	}
	if info.Interval != 5*time.Minute {
		t.Errorf("interval = %v, want the configured 5m", info.Interval)
	}
}

// TestServeOpensTheStoreReadOnly is the invariant that lets a dashboard run
// beside a collecting daemon: the serving handle cannot write, and the refusal
// is the store's own rather than a driver error three layers down.
func TestServeOpensTheStoreReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	st.Close()

	ro, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()

	if err := ro.RebuildRollup(context.Background()); err == nil {
		t.Error("a read-only handle accepted a write")
	}
}

// TestServeRunsTheAPIOverLoopback boots the actual server the command builds and
// asks it for /api/meta, so the wiring (read-only store, options, routes) is
// exercised end to end rather than only in the web package's own tests.
func TestServeRunsTheAPIOverLoopback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("create db: %v", err)
	}
	st.Close()

	ro, err := store.OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer ro.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv, err := web.New(ro, web.Options{DBPath: path, ServerVersion: buildinfo.Identity()})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ServeListener(ctx, ln) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/api/meta")
	if err != nil {
		cancel()
		t.Fatalf("get meta: %v", err)
	}
	body := new(bytes.Buffer)
	_, _ = body.ReadFrom(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("meta = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(body.String(), `"contract_version"`) {
		cancel()
		t.Fatalf("meta body = %s, want the contract version", body.String())
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not shut down when its context was cancelled")
	}
}

// TestServeGuidanceMentionsDoctor keeps the two capability surfaces pointing at
// each other: a user who cannot serve is told where to check what they have.
func TestServeGuidanceMentionsDoctor(t *testing.T) {
	if !strings.Contains(noEmbeddedUIGuidance, "aiusage doctor") {
		t.Error("the guidance does not point at doctor")
	}
}
