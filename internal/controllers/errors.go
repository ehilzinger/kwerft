package controllers

import (
	"errors"
	"fmt"
)

// terminalError is a problem only the user can fix (bad input, a name clash).
// Reconcilers report it in the Ready condition and do not retry.
type terminalError struct {
	reason string
	msg    string
}

func (e *terminalError) Error() string { return e.msg }

func terminalf(reason, format string, args ...any) error {
	return &terminalError{reason: reason, msg: fmt.Sprintf(format, args...)}
}

func isTerminal(err error) bool {
	var t *terminalError
	return errors.As(err, &t)
}

// reasonOf maps an error to a Ready condition reason.
func reasonOf(err error) string {
	var t *terminalError
	if errors.As(err, &t) {
		return t.reason
	}
	return "ReconcileFailed"
}
