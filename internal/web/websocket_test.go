package web

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestLiveChannelHandshakeAndFrames drives the real protocol over a real socket:
// upgrade, receive the join frame, then receive the frame a collection cycle
// produces. If the framing is wrong, no browser will ever tell us politely.
func TestLiveChannelHandshakeAndFrames(t *testing.T) {
	srv, path := newTestServer(t, defaultEvents())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.watch(ctx)
	// Let the watcher take its baseline first. A change that lands before its
	// first poll is absorbed into that baseline by design (an existing database
	// is not news), and this test is about the change AFTER it.
	time.Sleep(50 * time.Millisecond)

	conn, br := dialWS(t, ts.Listener.Addr().String(), "")
	defer conn.Close()

	// The join frame states the position now, so a page that connects between
	// cycles is not blind until the next one.
	join := readLiveFrame(t, br)
	if want := seedTime.Add(2*time.Hour + time.Minute).Unix(); join.Watermark != want {
		t.Errorf("join frame watermark = %d, want %d", join.Watermark, want)
	}

	at := time.Now().Add(time.Second).Truncate(time.Second)
	touch(t, path+"-wal", at)

	cycle := readLiveFrame(t, br)
	if cycle.CycleAt != at.Unix() {
		t.Errorf("cycle_at = %d, want the write time %d", cycle.CycleAt, at.Unix())
	}
}

// TestLiveFrameCarriesOnlyTheNotification pins the frame's shape: two fields,
// nothing else. The channel is an invalidation signal, and a frame that grew
// data would put the ledger on a socket nobody is paging.
func TestLiveFrameCarriesOnlyTheNotification(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	conn, br := dialWS(t, ts.Listener.Addr().String(), "")
	defer conn.Close()

	opcode, payload, err := readTestFrame(br)
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if opcode != opText {
		t.Fatalf("opcode = %#x, want text", opcode)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatalf("decode frame %q: %v", payload, err)
	}
	if len(fields) != 2 {
		t.Fatalf("frame = %v, want exactly watermark and cycle_at", fields)
	}
	for _, k := range []string{"watermark", "cycle_at"} {
		if _, ok := fields[k]; !ok {
			t.Errorf("frame is missing %q: %v", k, fields)
		}
	}
	assertNoRaw(t, payload)
}

// TestLiveChannelAnswersPing keeps the connection alive through a proxy and
// proves the control-frame path works in both directions.
func TestLiveChannelAnswersPing(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	conn, br := dialWS(t, ts.Listener.Addr().String(), "")
	defer conn.Close()
	readLiveFrame(t, br) // the join frame

	if err := writeClientFrame(conn, opPing, []byte("hello")); err != nil {
		t.Fatalf("send ping: %v", err)
	}
	opcode, payload, err := readTestFrame(br)
	if err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if opcode != opPong || string(payload) != "hello" {
		t.Errorf("got opcode %#x payload %q, want a pong echoing the payload", opcode, payload)
	}
}

// TestLiveChannelClosesOnClientClose: a page that navigated away is deregistered
// rather than accumulating in the hub.
func TestLiveChannelClosesOnClientClose(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	conn, br := dialWS(t, ts.Listener.Addr().String(), "")
	readLiveFrame(t, br)
	if srv.hub.count() != 1 {
		t.Fatalf("clients = %d, want 1", srv.hub.count())
	}

	if err := writeClientFrame(conn, opClose, []byte{0x03, 0xE8}); err != nil {
		t.Fatalf("send close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for srv.hub.count() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.hub.count() != 0 {
		t.Errorf("clients = %d after a client close, want 0", srv.hub.count())
	}
	conn.Close()
}

// TestLiveChannelRejectsAForeignOrigin: a browser will open a socket to a
// loopback port on behalf of any page the user is visiting, and unlike a fetch,
// nothing stops it reading the reply.
func TestLiveChannelRejectsAForeignOrigin(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	addr := ts.Listener.Addr().String()
	status := handshakeStatus(t, addr, "https://evil.example")
	if status != http.StatusForbidden {
		t.Errorf("foreign origin handshake = %d, want 403", status)
	}
	// The dev deployment proxies from a loopback Vite server, and the browser
	// sends that origin; refusing it would break the development loop.
	if status := handshakeStatus(t, addr, "http://127.0.0.1:5173"); status != http.StatusSwitchingProtocols {
		t.Errorf("loopback origin handshake = %d, want 101", status)
	}
	// A rebinding client's Origin agrees with its Host, because it controls
	// both. The Host guard refuses it first; the origin rule would refuse it
	// too, and neither may be satisfied by that agreement.
	if status := handshakeStatusAs(t, addr, "rebind.evil", "http://rebind.evil"); status != http.StatusMisdirectedRequest {
		t.Errorf("rebinding handshake = %d, want 421", status)
	}
	// No Origin at all is a non-browser client and stays welcome.
	if status := handshakeStatus(t, addr, ""); status != http.StatusSwitchingProtocols {
		t.Errorf("originless handshake = %d, want 101", status)
	}
}

// TestNonUpgradeRequestIsRefused: /api/ws over plain HTTP is a client mistake
// worth naming.
func TestNonUpgradeRequestIsRefused(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	if rec := get(t, srv, "/api/ws"); rec.Code != http.StatusBadRequest {
		t.Errorf("plain GET /api/ws = %d, want 400", rec.Code)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/ws", nil)
	req.Host = testHost
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Sec-WebSocket-Version", "8")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("version 8 handshake = %d, want 400", rec.Code)
	}
}

// TestAcceptKey pins the handshake against two published key/accept pairs. It
// is the one test here that must NOT be written from this package's own
// arithmetic: the magic GUID is a 36-character constant nobody can eyeball, and
// a single transposed digit yields a handshake every browser refuses without
// ever saying why. (It caught exactly that while this package was written.)
func TestAcceptKey(t *testing.T) {
	tests := []struct{ key, accept string }{
		{"dGhlIHNhbXBsZSBub25jZQ==", "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="},
		{"x3JJHMbDL1EzLkh9GBhXDw==", "HSmrc0sMlYUkAGmm5OPpG2HaGWk="},
	}
	for _, tc := range tests {
		if got := acceptKey(tc.key); got != tc.accept {
			t.Errorf("acceptKey(%q) = %q, want %q", tc.key, got, tc.accept)
		}
	}
}

// TestFrameLengthEncodings walks the three payload-length forms the protocol
// has. Getting the 126/127 boundary wrong produces a stream that decodes as
// garbage only for large frames, which is the worst kind of bug to find live.
func TestFrameLengthEncodings(t *testing.T) {
	for _, n := range []int{0, 1, 125, 126, 127, 65535, 65536, 70000} {
		payload := make([]byte, n)
		for i := range payload {
			payload[i] = byte('a' + i%26)
		}
		var buf strings.Builder
		if err := writeWSFrame(&buf, opText, payload); err != nil {
			t.Fatalf("write %d-byte frame: %v", n, err)
		}
		opcode, got, err := readTestFrame(bufio.NewReader(strings.NewReader(buf.String())))
		if err != nil {
			t.Fatalf("read %d-byte frame: %v", n, err)
		}
		if opcode != opText || string(got) != string(payload) {
			t.Fatalf("%d-byte frame round trip lost data", n)
		}
	}
}

// TestReadWSFrameRefusesUnmaskedAndOversized covers the two refusals the RFC
// demands of a server.
func TestReadWSFrameRefusesUnmaskedAndOversized(t *testing.T) {
	var unmasked strings.Builder
	if err := writeWSFrame(&unmasked, opText, []byte("hi")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, _, err := readWSFrame(strings.NewReader(unmasked.String())); !errors.Is(err, errUnmasked) {
		t.Errorf("unmasked client frame error = %v, want errUnmasked", err)
	}

	// A masked header claiming a payload beyond the cap, with no payload behind
	// it: the length must be refused before anything is allocated.
	oversized := []byte{0x81, 0xFF}
	oversized = binary.BigEndian.AppendUint64(oversized, uint64(maxClientFrame+1))
	if _, _, err := readWSFrame(strings.NewReader(string(oversized))); !errors.Is(err, errFrameTooLarge) {
		t.Errorf("oversized frame error = %v, want errFrameTooLarge", err)
	}
}

// TestOriginAllowed pins the live channel's origin rule against the allowlist.
// The case that matters most is the last one: under DNS rebinding the attacker
// controls the name, so Origin and Host AGREE - a check that compared them
// would pass the attack through, which is why it compares against the
// allowlist instead.
func TestOriginAllowed(t *testing.T) {
	srv, _ := newTestServer(t, defaultEvents())
	srv.hosts = newHostSet([]string{"aiusage.example"})

	tests := []struct {
		name   string
		origin string
		host   string
		want   bool
	}{
		{"absent origin is a non-browser client", "", "127.0.0.1:37800", true},
		{"same origin", "http://127.0.0.1:37800", "127.0.0.1:37800", true},
		{"proxied name in the allowlist", "https://aiusage.example", "aiusage.example", true},
		{"vite dev server", "http://localhost:5173", "127.0.0.1:37801", true},
		{"loopback ipv6", "http://[::1]:5173", "127.0.0.1:37801", true},
		{"foreign page", "https://evil.example", "127.0.0.1:37800", false},
		{"rebinding: origin matches host, neither is allowed", "http://rebind.evil", "rebind.evil", false},
		{"unparseable", "::::", "127.0.0.1:37800", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.host+"/api/ws", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if got := srv.originAllowed(r); got != tc.want {
				t.Fatalf("originAllowed(origin=%q host=%q) = %v, want %v", tc.origin, tc.host, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- test client

// wsHandshake is the client half of the upgrade, written by hand so the test
// exercises the wire and not a library that agrees with our assumptions. host
// is sent verbatim, so a test can present a Host the server never bound.
func wsHandshake(host, origin string) string {
	req := "GET /api/ws HTTP/1.1\r\n" +
		"Host: " + host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n" +
		"Sec-WebSocket-Version: 13\r\n"
	if origin != "" {
		req += "Origin: " + origin + "\r\n"
	}
	return req + "\r\n"
}

// dialWS opens a live channel and returns the connection past the handshake.
func dialWS(t *testing.T, addr, origin string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if _, err := io.WriteString(conn, wsHandshake(addr, origin)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	br := bufio.NewReader(conn)
	status, headers := readHandshakeResponse(t, br)
	if status != http.StatusSwitchingProtocols {
		conn.Close()
		t.Fatalf("handshake status = %d, want 101", status)
	}
	if got := headers["sec-websocket-accept"]; got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		conn.Close()
		t.Fatalf("Sec-WebSocket-Accept = %q, want the accept for the published key", got)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, br
}

// handshakeStatus performs only the handshake and reports its status code.
func handshakeStatus(t *testing.T, addr, origin string) int {
	t.Helper()
	return handshakeStatusAs(t, addr, addr, origin)
}

// handshakeStatusAs is handshakeStatus with a Host of the caller's choosing,
// which is what a rebinding client presents: a name it controls, resolved to
// the loopback address it is really talking to.
func handshakeStatusAs(t *testing.T, addr, host, origin string) int {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, wsHandshake(host, origin)); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	status, _ := readHandshakeResponse(t, bufio.NewReader(conn))
	return status
}

// readHandshakeResponse parses the status line and headers by hand: the stdlib
// response reader would treat the hijacked connection as a body and swallow the
// frames the test is here to read.
func readHandshakeResponse(t *testing.T, br *bufio.Reader) (int, map[string]string) {
	t.Helper()
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	var proto string
	var code int
	if _, err := fmt.Sscanf(statusLine, "%s %d", &proto, &code); err != nil {
		t.Fatalf("parse status line %q: %v", statusLine, err)
	}
	headers := map[string]string{}
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read header: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			return code, headers
		}
		name, value, ok := strings.Cut(line, ":")
		if ok {
			headers[strings.ToLower(strings.TrimSpace(name))] = strings.TrimSpace(value)
		}
	}
}

// readTestFrame reads one server frame (always unmasked, per the RFC).
func readTestFrame(br *bufio.Reader) (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(br, head[:]); err != nil {
		return 0, nil, err
	}
	if head[1]&0x80 != 0 {
		return 0, nil, errors.New("server frame is masked")
	}
	length := int64(head[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	return head[0] & 0x0F, payload, nil
}

// readLiveFrame reads the next text frame and decodes it, skipping any control
// frame the server may have interleaved.
func readLiveFrame(t *testing.T, br *bufio.Reader) liveFrame {
	t.Helper()
	for {
		opcode, payload, err := readTestFrame(br)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if opcode != opText {
			continue
		}
		var f liveFrame
		if err := json.Unmarshal(payload, &f); err != nil {
			t.Fatalf("decode frame %q: %v", payload, err)
		}
		return f
	}
}

// writeClientFrame writes a masked client frame, which is what a real client
// must send and what the server's reader is written to expect.
func writeClientFrame(w io.Writer, opcode byte, payload []byte) error {
	header := []byte{0x80 | opcode}
	n := len(payload)
	switch {
	case n <= 125:
		header = append(header, 0x80|byte(n))
	case n <= 0xFFFF:
		header = append(header, 0x80|126)
		header = binary.BigEndian.AppendUint16(header, uint16(n))
	default:
		header = append(header, 0x80|127)
		header = binary.BigEndian.AppendUint64(header, uint64(n))
	}
	mask := []byte{0x12, 0x34, 0x56, 0x78}
	header = append(header, mask...)
	masked := make([]byte, n)
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(masked)
	return err
}

// fakeConn is a net.Conn that goes nowhere, for hub tests that never speak the
// protocol.
type fakeConn struct{ closed bool }

func (c *fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *fakeConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *fakeConn) Close() error                     { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }
