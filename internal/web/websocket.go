package web

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// A minimal server half of RFC 6455, because that is all this endpoint needs:
// it pushes small JSON text frames and reads nothing but control frames. The
// whole client protocol - subprotocols, extensions, permessage-deflate,
// fragmented sends, binary payloads - is unused here, so a dependency would be
// several thousand lines carried for a handshake and a two-field message.
//
// What is implemented is implemented properly: the handshake is validated,
// client frames are unmasked as the RFC requires, control frames are answered,
// oversized frames are refused, and exactly one goroutine ever writes to the
// socket.

const (
	// wsGUID is the fixed string RFC 6455 concatenates with the client key. The
	// SHA-1 below is a protocol handshake, not a security primitive: it proves
	// the peer speaks WebSocket, nothing more. One transposed hex digit here
	// produces a handshake every browser silently refuses, which is why
	// TestAcceptKey pins it against published key/accept pairs rather than
	// against this package's own arithmetic.
	wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

	opText  = 0x1
	opClose = 0x8
	opPing  = 0x9
	opPong  = 0xA

	// maxClientFrame caps an inbound payload. The page never sends a data frame
	// and control frames are capped at 125 bytes by the RFC, so anything near
	// this is already a client doing something it was not asked to.
	maxClientFrame = 4096

	// sendQueue is how many undelivered frames a client may have outstanding.
	// Frames are notifications, not a stream: a client this far behind gains
	// nothing from the backlog, and dropping it is the recovery path its
	// reconnect loop already implements.
	sendQueue = 8

	// pingInterval keeps idle connections alive through reverse proxies and is
	// how a peer that vanished without a FIN is eventually noticed - the write
	// fails and the connection is dropped.
	pingInterval = 30 * time.Second
)

// handleWS serves WS /api/ws: the live channel. It upgrades the connection and
// then only ever pushes; the client is not asked for anything.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, http.StatusMethodNotAllowed, "read-only api: use GET")
		return
	}
	if !isUpgradeRequest(r) {
		writeError(w, http.StatusBadRequest, "not a websocket upgrade")
		return
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		writeError(w, http.StatusBadRequest, "unsupported websocket version")
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "missing websocket key")
		return
	}
	// A browser will happily open a socket to a loopback port on behalf of any
	// page the user is visiting; unlike a fetch, nothing stops it reading the
	// reply. The frames carry only a watermark and a timestamp, but the check
	// costs one header and keeps a stranger's page off this port.
	if !s.originAllowed(r) {
		writeError(w, http.StatusForbidden, "origin not allowed")
		return
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		writeError(w, http.StatusInternalServerError, "connection cannot be upgraded")
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		s.log.Printf("web: hijack: %v", err)
		return
	}

	if err := writeHandshake(conn, key); err != nil {
		s.log.Printf("web: websocket handshake: %v", err)
		conn.Close()
		return
	}

	c := newWSConn(conn, brw.Reader)
	accepted, replayed := s.hub.add(c)
	if !accepted {
		// The server is shutting down. Nothing will ever be sent on this
		// connection, so say so now instead of leaving it open on a hub that has
		// forgotten it.
		conn.Close()
		return
	}
	go c.writeLoop()
	go func() {
		c.readLoop()
		s.hub.remove(c)
	}()
	if !replayed {
		// Nothing has been broadcast yet, so state the position as it is now. A
		// client must never have to wait a whole collection interval to learn
		// what it connected to.
		watermark, err := s.reader.IngestWatermark(r.Context())
		if err != nil {
			s.log.Printf("web: read ingest watermark: %v", err)
			return
		}
		c.sendFrame(liveFrame{
			Watermark: unixOrZero(watermark),
			CycleAt:   unixOrZero(newestMTime(s.opt.DBPath)),
		})
	}
}

// isUpgradeRequest reports whether the request asks for a websocket upgrade.
// Both header values are token lists and both are case-insensitive, which is
// exactly the sort of detail a hand-rolled check gets wrong.
func isUpgradeRequest(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// originAllowed applies the same-origin rule browsers do not apply to
// WebSockets. An absent Origin is a non-browser client (curl, a test, another
// program) and is allowed; a browser origin must name a host in the SAME
// allowlist the Host guard uses - which covers the Vite dev server proxying
// from localhost:5173 as an ordinary consequence rather than a special case.
//
// Matching the Origin against the request's own Host would be worse than no
// check at all here: under DNS rebinding the attacker controls both, so they
// agree by construction and the check would wave the attack through.
func (s *Server) originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return s.hosts.allows(u.Host)
}

// writeHandshake sends the 101 response that completes the upgrade.
func writeHandshake(conn net.Conn, key string) error {
	accept := acceptKey(key)
	response := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if _, err := io.WriteString(conn, response); err != nil {
		return err
	}
	return conn.SetWriteDeadline(time.Time{})
}

// acceptKey computes Sec-WebSocket-Accept from the client's key.
func acceptKey(key string) string {
	h := sha1.New()
	_, _ = io.WriteString(h, key+wsGUID)
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// wsMessage is one frame queued for the writer.
type wsMessage struct {
	opcode byte
	data   []byte
}

// wsConn is one connected live client.
type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
	out  chan wsMessage

	// wmu serialises writes. The writer goroutine is the only routine that
	// writes frames, except for the courtesy close frame, and two writers
	// interleaving mid-frame would corrupt the stream.
	wmu       sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
}

func newWSConn(conn net.Conn, br *bufio.Reader) *wsConn {
	return &wsConn{
		conn: conn,
		br:   br,
		out:  make(chan wsMessage, sendQueue),
		done: make(chan struct{}),
	}
}

// send queues a text frame without blocking, reporting whether it was accepted.
// A full queue is a false, and the caller drops the client - the watcher must
// never wait on a socket.
func (c *wsConn) send(payload []byte) bool {
	select {
	case <-c.done:
		return false
	case c.out <- wsMessage{opcode: opText, data: payload}:
		return true
	default:
		return false
	}
}

// sendFrame queues one live frame, encoding it for this connection alone (the
// hub encodes once for a broadcast; this is the join-time frame).
func (c *wsConn) sendFrame(f liveFrame) {
	payload, err := encodeFrame(f)
	if err != nil {
		return
	}
	c.send(payload)
}

// close cuts the connection once. It sends a courtesy close frame first so a
// well-behaved client sees a clean shutdown instead of a reset.
func (c *wsConn) close() {
	c.closeOnce.Do(func() {
		close(c.done)
		c.wmu.Lock()
		_ = c.conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = writeWSFrame(c.conn, opClose, closeNormalPayload())
		c.wmu.Unlock()
		_ = c.conn.Close()
	})
}

// closeNormalPayload is a close frame body carrying status 1000 (normal).
func closeNormalPayload() []byte {
	return []byte{0x03, 0xE8}
}

// writeLoop owns the socket's write side until the connection ends.
func (c *wsConn) writeLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case m := <-c.out:
			if err := c.write(m.opcode, m.data); err != nil {
				c.close()
				return
			}
		case <-ticker.C:
			if err := c.write(opPing, nil); err != nil {
				c.close()
				return
			}
		}
	}
}

// write emits one frame under the write lock and a deadline, so a client that
// has stopped reading cannot pin the writer goroutine forever.
func (c *wsConn) write(opcode byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return writeWSFrame(c.conn, opcode, payload)
}

// readLoop consumes client frames until the connection ends. Nothing the client
// sends carries meaning here, but the read side cannot simply be ignored:
// control frames must be answered, and a close frame is how a page that
// navigated away says so.
//
// There is deliberately no read deadline. The page can sit silent for hours
// between cycles, and a peer that vanished is noticed by the ping write failing
// rather than by punishing a client for having nothing to say.
func (c *wsConn) readLoop() {
	defer c.close()
	for {
		opcode, payload, err := readWSFrame(c.br)
		if err != nil {
			return
		}
		switch opcode {
		case opClose:
			return
		case opPing:
			select {
			case c.out <- wsMessage{opcode: opPong, data: payload}:
			case <-c.done:
				return
			default:
				// Queue full: the connection is already being dropped for
				// falling behind, and a pong will not save it.
			}
		}
	}
}

// writeWSFrame writes one unmasked server frame. Server-to-client frames are
// never masked (RFC 6455 section 5.1).
func writeWSFrame(w io.Writer, opcode byte, payload []byte) error {
	header := make([]byte, 0, 10)
	header = append(header, 0x80|opcode) // FIN set: this package never fragments.

	n := len(payload)
	switch {
	case n <= 125:
		header = append(header, byte(n))
	case n <= 0xFFFF:
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(n))
	default:
		header = append(header, 127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(n))
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	_, err := w.Write(payload)
	return err
}

// errFrameTooLarge refuses an inbound frame beyond maxClientFrame.
var errFrameTooLarge = errors.New("web: websocket frame too large")

// errUnmasked refuses an unmasked client frame, which RFC 6455 section 5.1
// requires the server to treat as a protocol error.
var errUnmasked = errors.New("web: unmasked client frame")

// readWSFrame reads one client frame and returns its opcode and unmasked
// payload. Fragmentation is not reassembled: a continuation arrives as its own
// frame with opcode 0 and is ignored by the caller, which is correct here
// because no client data frame means anything to this endpoint.
func readWSFrame(r io.Reader) (byte, []byte, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	opcode := head[0] & 0x0F
	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7F)

	switch length {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return 0, nil, err
		}
		length = int64(binary.BigEndian.Uint64(ext[:]) & 0x7FFFFFFFFFFFFFFF)
	}
	if length > maxClientFrame {
		return 0, nil, errFrameTooLarge
	}
	if !masked {
		return 0, nil, errUnmasked
	}

	var mask [4]byte
	if _, err := io.ReadFull(r, mask[:]); err != nil {
		return 0, nil, err
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return opcode, payload, nil
}
