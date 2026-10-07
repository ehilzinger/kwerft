// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package clusters

import (
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// wsConn is a net.Conn over a WebSocket: every Write is one binary message,
// and Read returns the messages' bytes in order. HTTP/2 runs on top of it
// (tunnel.go), so message boundaries carry no meaning.
type wsConn struct {
	ws *websocket.Conn

	rmu sync.Mutex
	r   io.Reader // the current message, nil between messages

	wmu sync.Mutex

	closeOnce sync.Once
	done      chan struct{}
}

// maxMessage bounds one WebSocket message. HTTP/2 frames are at most 16 KiB
// by default (plus a header), and the framer's buffered writer flushes
// smaller writes, so this is generous.
const maxMessage = 4 << 20

func newWSConn(ws *websocket.Conn) *wsConn {
	ws.SetReadLimit(maxMessage)
	return &wsConn{ws: ws, done: make(chan struct{})}
}

func (c *wsConn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for {
		if c.r == nil {
			kind, r, err := c.ws.NextReader()
			if err != nil {
				c.Close()
				return 0, err
			}
			if kind != websocket.BinaryMessage {
				continue // nothing but binary messages is part of the stream
			}
			c.r = r
		}
		n, err := c.r.Read(p)
		if err == io.EOF {
			c.r = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		if err != nil {
			c.Close()
		}
		return n, err
	}
}

func (c *wsConn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		c.Close()
		return 0, err
	}
	return len(p), nil
}

// Close closes the WebSocket (with a close message when it still can) and
// marks the connection done.
func (c *wsConn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		// WriteControl and Close may run concurrently with a blocked Write.
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		err = c.ws.Close()
	})
	return err
}

// Done is closed once the connection is closed or broken.
func (c *wsConn) Done() <-chan struct{} { return c.done }

func (c *wsConn) LocalAddr() net.Addr  { return c.ws.LocalAddr() }
func (c *wsConn) RemoteAddr() net.Addr { return c.ws.RemoteAddr() }

func (c *wsConn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}

func (c *wsConn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *wsConn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
