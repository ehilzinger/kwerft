package server

import (
	"errors"
	"fmt"
	"testing"

	"k8s.io/apimachinery/pkg/util/httpstream"
	streaming "k8s.io/streaming/pkg/httpstream"
)

// client-go reports a refused WebSocket upgrade with k8s.io/streaming's
// error type; the shell must fall back to SPDY on it.
func TestShellFallsBackOnRefusedUpgrade(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("exec: %w", &streaming.UpgradeFailureError{Cause: errors.New("bad handshake")}),
		&httpstream.UpgradeFailureError{Cause: errors.New("bad handshake")},
	} {
		if !shouldFallBack(err) {
			t.Errorf("no fallback on %v", err)
		}
	}
	if shouldFallBack(errors.New("connection refused")) {
		t.Error("fallback on an unrelated error")
	}
}
