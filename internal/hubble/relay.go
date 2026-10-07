// SPDX-FileCopyrightText: 2026 Enzo Hilzinger
// SPDX-License-Identifier: AGPL-3.0-only

package hubble

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultRelayAddress is the relay Service Cilium's chart creates (plain
// gRPC: Cilium's default leaves the relay's own server without TLS; the
// relay talks mTLS to the agents).
const DefaultRelayAddress = "hubble-relay.kube-system.svc:80"

// maxMessage bounds one flow message from the relay.
const maxMessage = 4 << 20

// Source streams flows; Relay is the real one.
type Source interface {
	// Flows streams flows from since on to sink and follows new ones until
	// ctx ends or the stream breaks.
	Flows(ctx context.Context, since time.Time, sink Sink) error
}

// Sink receives a flow stream. Every field is optional.
type Sink struct {
	Connected func()       // the relay accepted the request
	Flow      func(*Flow)  // one flow
	Lost      func(uint64) // flows the relay could not deliver
}

// Relay is a client for the Hubble relay's Observer.GetFlows: gRPC over
// HTTP/2 without TLS (h2c), spoken with net/http.
type Relay struct {
	base   *url.URL
	client *http.Client
}

// NewRelay returns a client for the relay at addr (host:port).
func NewRelay(addr string) *Relay {
	tr := &http.Transport{
		Protocols:           new(http.Protocols),
		MaxIdleConns:        1,
		IdleConnTimeout:     time.Minute,
		TLSHandshakeTimeout: 10 * time.Second,
		// Pings keep a long, quiet stream from being cut by NAT or conntrack.
		HTTP2: &http.HTTP2Config{SendPingTimeout: 30 * time.Second, PingTimeout: 15 * time.Second},
	}
	tr.Protocols.SetUnencryptedHTTP2(true)
	return &Relay{base: &url.URL{Scheme: "http", Host: addr}, client: &http.Client{Transport: tr}}
}

func (r *Relay) Flows(ctx context.Context, since time.Time, sink Sink) error {
	msg := encodeGetFlowsRequest(since)
	body := make([]byte, 5+len(msg))
	binary.BigEndian.PutUint32(body[1:5], uint32(len(msg)))
	copy(body[5:], msg)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base.JoinPath("/observer.Observer/GetFlows").String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/grpc+proto")
	req.Header.Set("TE", "trailers")
	res, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("hubble relay: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("hubble relay: HTTP %s", res.Status)
	}
	if !strings.HasPrefix(res.Header.Get("Content-Type"), "application/grpc") {
		return fmt.Errorf("hubble relay: unexpected content type %q", res.Header.Get("Content-Type"))
	}
	// A trailers-only answer (an immediate error) carries the status in the
	// headers.
	if err := grpcStatus(res.Header); err != nil {
		return err
	}
	if sink.Connected != nil {
		sink.Connected()
	}

	rd := bufio.NewReaderSize(res.Body, 64<<10)
	var head [5]byte
	buf := make([]byte, 0, 4096)
	for {
		if _, err := io.ReadFull(rd, head[:]); err != nil {
			if errors.Is(err, io.EOF) {
				if serr := grpcStatus(res.Trailer); serr != nil {
					return serr
				}
				return io.EOF // the stream ended; follow mode should not
			}
			return fmt.Errorf("hubble relay: %w", err)
		}
		if head[0] != 0 {
			return errors.New("hubble relay: compressed messages are not supported")
		}
		n := binary.BigEndian.Uint32(head[1:])
		if n > maxMessage {
			return fmt.Errorf("hubble relay: message of %d bytes is too large", n)
		}
		buf = buf[:0]
		buf = append(buf, make([]byte, n)...)
		if _, err := io.ReadFull(rd, buf); err != nil {
			return fmt.Errorf("hubble relay: %w", err)
		}
		resp, err := decodeResponse(buf)
		if err != nil {
			return fmt.Errorf("hubble relay: %w", err)
		}
		switch {
		case resp.flow != nil && sink.Flow != nil:
			sink.Flow(resp.flow)
		case resp.lost > 0 && sink.Lost != nil:
			sink.Lost(resp.lost)
		}
	}
}

func grpcStatus(h http.Header) error {
	code := h.Get("Grpc-Status")
	if code == "" || code == "0" {
		return nil
	}
	msg, _ := url.PathUnescape(h.Get("Grpc-Message"))
	return &grpcError{code: code, message: msg}
}
