package wa

import "errors"

// ErrNotSentStale proves the message did not reach the socket.
var ErrNotSentStale = staleSendError{}

// ErrMaybeSent marks a send whose delivery cannot safely be retried.
var ErrMaybeSent = errors.New("unknown_maybe_sent")

// MaybeSentError retains the generated ID even when the acknowledgement is lost.
type MaybeSentError struct{ MessageID string }

func (e *MaybeSentError) Error() string { return "unknown_maybe_sent" }
func (e *MaybeSentError) Unwrap() error { return ErrMaybeSent }

type staleSendError struct{}

func (staleSendError) Error() string { return "not_sent: whatsapp connection stale" }
func (staleSendError) Unwrap() error { return ErrNotSent }
