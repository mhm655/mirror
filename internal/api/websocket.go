package api

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// A minimal RFC 6455 server endpoint.
//
// Written rather than imported. The justification is narrow and specific: this
// server sends one kind of message -- a server-to-client binary frame -- and
// needs precise control over buffer reuse on that path, because the world
// frame is rebuilt and broadcast up to ten times a second to every connected
// client and an allocation per frame per client is the difference between a
// flat and a sawtooth heap profile. The general-purpose libraries are fine;
// they just carry a permessage-deflate negotiator, an extension registry and a
// client dialer that this process will never use.
//
// What is implemented: the opening handshake, binary/text/ping/pong/close
// frames, client-to-server unmasking, fragmentation, and the 125-byte control
// frame limit. What is not: extensions, subprotocol negotiation beyond echoing
// one back, and the client role. Anything unsupported is rejected explicitly
// rather than ignored.

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type opcode byte

const (
	opContinuation opcode = 0x0
	opText         opcode = 0x1
	opBinary       opcode = 0x2
	opClose        opcode = 0x8
	opPing         opcode = 0x9
	opPong         opcode = 0xA
)

// Close status codes we actually send.
const (
	closeNormal        = 1000
	closeGoingAway     = 1001
	closeProtocolError = 1002
	closeTooBig        = 1009
)

// Conn is one upgraded connection.
type Conn struct {
	conn net.Conn
	br   *bufio.Reader

	// One writer at a time. Broadcasts come from the hub goroutine and pongs
	// come from the reader goroutine, so the mutex is load-bearing rather than
	// defensive.
	wmu  sync.Mutex
	wbuf []byte

	closeOnce sync.Once
	closed    chan struct{}

	MaxMessage int64
	WriteWait  time.Duration
	// Subprotocol echoed during the handshake, if any.
	Subprotocol string
}

var errNotWebsocket = errors.New("websocket: not an upgrade request")

// Upgrade performs the opening handshake and hijacks the connection.
func Upgrade(w http.ResponseWriter, r *http.Request, subprotocols ...string) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!headerContainsToken(r.Header.Get("Connection"), "upgrade") {
		return nil, errNotWebsocket
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		w.Header().Set("Sec-WebSocket-Version", "13")
		http.Error(w, "unsupported websocket version", http.StatusUpgradeRequired)
		return nil, errors.New("websocket: version must be 13")
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" {
		http.Error(w, "missing Sec-WebSocket-Key", http.StatusBadRequest)
		return nil, errors.New("websocket: missing key")
	}

	chosen := ""
	if offered := r.Header.Get("Sec-WebSocket-Protocol"); offered != "" && len(subprotocols) > 0 {
		for _, o := range strings.Split(offered, ",") {
			o = strings.TrimSpace(o)
			for _, s := range subprotocols {
				if o == s {
					chosen = s
					break
				}
			}
			if chosen != "" {
				break
			}
		}
	}

	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("websocket: response writer does not support hijacking")
	}
	netConn, brw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}

	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])

	var b strings.Builder
	b.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n")
	if chosen != "" {
		b.WriteString("Sec-WebSocket-Protocol: " + chosen + "\r\n")
	}
	b.WriteString("\r\n")
	if _, err := netConn.Write([]byte(b.String())); err != nil {
		netConn.Close()
		return nil, err
	}

	c := &Conn{
		conn: netConn, br: brw.Reader,
		closed:      make(chan struct{}),
		MaxMessage:  1 << 20,
		WriteWait:   10 * time.Second,
		Subprotocol: chosen,
		wbuf:        make([]byte, 0, 64*1024),
	}
	return c, nil
}

func headerContainsToken(h, token string) bool {
	for _, part := range strings.Split(h, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// WriteBinary sends one unfragmented binary frame.
//
// The frame header is written into the same reusable buffer as the payload so
// the whole frame goes out in a single Write syscall. Two writes per frame per
// client at 10 Hz is not a lot on its own, but it is a lot of small packets
// once you have a few hundred clients and Nagle is off.
func (c *Conn) WriteBinary(payload []byte) error { return c.write(opBinary, payload) }

// WriteText sends one unfragmented text frame.
func (c *Conn) WriteText(payload []byte) error { return c.write(opText, payload) }

func (c *Conn) write(op opcode, payload []byte) error {
	select {
	case <-c.closed:
		return io.ErrClosedPipe
	default:
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()

	n := len(payload)
	c.wbuf = c.wbuf[:0]
	c.wbuf = append(c.wbuf, 0x80|byte(op)) // FIN set, no RSV
	switch {
	case n < 126:
		c.wbuf = append(c.wbuf, byte(n))
	case n <= 0xFFFF:
		c.wbuf = append(c.wbuf, 126, byte(n>>8), byte(n))
	default:
		var ext [8]byte
		binary.BigEndian.PutUint64(ext[:], uint64(n))
		c.wbuf = append(c.wbuf, 127)
		c.wbuf = append(c.wbuf, ext[:]...)
	}
	c.wbuf = append(c.wbuf, payload...)

	if c.WriteWait > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.WriteWait))
	}
	_, err := c.conn.Write(c.wbuf)
	if err != nil {
		c.closeNow()
	}
	return err
}

// Message is one received application frame.
type Message struct {
	Binary bool
	Data   []byte
}

// ReadMessage blocks for the next application frame, transparently answering
// pings and honouring close frames.
func (c *Conn) ReadMessage() (Message, error) {
	var (
		payload []byte
		msgOp   opcode
		started bool
	)
	for {
		fin, op, data, err := c.readFrame()
		if err != nil {
			return Message{}, err
		}
		switch op {
		case opPing:
			if err := c.write(opPong, data); err != nil {
				return Message{}, err
			}
			continue
		case opPong:
			continue
		case opClose:
			code := closeNormal
			if len(data) >= 2 {
				code = int(binary.BigEndian.Uint16(data))
			}
			c.writeClose(code, "")
			c.closeNow()
			return Message{}, io.EOF
		case opContinuation:
			if !started {
				c.fail(closeProtocolError, "continuation without start")
				return Message{}, errors.New("websocket: unexpected continuation")
			}
		case opText, opBinary:
			if started {
				c.fail(closeProtocolError, "interleaved message")
				return Message{}, errors.New("websocket: interleaved data frame")
			}
			started, msgOp = true, op
		default:
			c.fail(closeProtocolError, "unknown opcode")
			return Message{}, fmt.Errorf("websocket: unknown opcode %x", op)
		}

		payload = append(payload, data...)
		if int64(len(payload)) > c.MaxMessage {
			c.fail(closeTooBig, "message too large")
			return Message{}, errors.New("websocket: message exceeds limit")
		}
		if fin {
			if msgOp == opText && !utf8.Valid(payload) {
				c.fail(closeProtocolError, "invalid utf-8")
				return Message{}, errors.New("websocket: invalid utf-8 in text frame")
			}
			return Message{Binary: msgOp == opBinary, Data: payload}, nil
		}
	}
}

func (c *Conn) readFrame() (fin bool, op opcode, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.br, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	if h[0]&0x70 != 0 {
		c.fail(closeProtocolError, "reserved bits set")
		err = errors.New("websocket: RSV bits set without a negotiated extension")
		return
	}
	op = opcode(h[0] & 0x0F)
	masked := h[1]&0x80 != 0
	length := int64(h[1] & 0x7F)

	if op >= opClose {
		// Control frames must be short and unfragmented.
		if length > 125 || !fin {
			c.fail(closeProtocolError, "malformed control frame")
			err = errors.New("websocket: malformed control frame")
			return
		}
	}
	switch length {
	case 126:
		var e [2]byte
		if _, err = io.ReadFull(c.br, e[:]); err != nil {
			return
		}
		length = int64(binary.BigEndian.Uint16(e[:]))
	case 127:
		var e [8]byte
		if _, err = io.ReadFull(c.br, e[:]); err != nil {
			return
		}
		v := binary.BigEndian.Uint64(e[:])
		if v > 1<<62 {
			err = errors.New("websocket: absurd frame length")
			return
		}
		length = int64(v)
	}
	if length > c.MaxMessage {
		c.fail(closeTooBig, "frame too large")
		err = errors.New("websocket: frame exceeds limit")
		return
	}
	// RFC 6455 5.1: a server MUST close the connection on an unmasked client
	// frame. Not enforcing this is a real (if unglamorous) proxy-poisoning
	// vector, so it is enforced.
	if !masked {
		c.fail(closeProtocolError, "client frame not masked")
		err = errors.New("websocket: client frame was not masked")
		return
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.br, mask[:]); err != nil {
		return
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i&3]
	}
	return
}

func (c *Conn) fail(code int, reason string) {
	c.writeClose(code, reason)
	c.closeNow()
}

func (c *Conn) writeClose(code int, reason string) {
	buf := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(buf, uint16(code))
	copy(buf[2:], reason)
	_ = c.write(opClose, buf)
}

// Ping sends a ping frame; the peer's pong is consumed by ReadMessage.
func (c *Conn) Ping() error { return c.write(opPing, nil) }

// Close shuts the connection down politely, then hard.
func (c *Conn) Close() error {
	c.writeClose(closeGoingAway, "server shutting down")
	c.closeNow()
	return nil
}

func (c *Conn) closeNow() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}

func (c *Conn) Done() <-chan struct{} { return c.closed }

func (c *Conn) RemoteAddr() string { return c.conn.RemoteAddr().String() }

// SetReadDeadline bounds how long a read may block, which is how a dead peer
// that never sends a close frame is eventually reaped.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.conn.SetReadDeadline(t) }
