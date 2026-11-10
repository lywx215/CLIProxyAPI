package executor

import (
	"errors"
	"net"
	"testing"
)

func TestObservedTransportFailurePreservesCauseAndReplaySafety(t *testing.T) {
	reset := errors.New("connection reset after write")
	unknown := TransportFailure(reset)
	if !errors.Is(unknown, reset) || !ReplayForbidden(unknown) || GenerationHasStarted(unknown) {
		t.Fatal("unknown acceptance lost its cause or replay barrier")
	}
	f := FactsFromError(unknown)
	if f.Generation != "unknown" || f.SendState != "possibly_sent" {
		t.Fatalf("facts = %+v", f)
	}
	dial := &net.OpError{Op: "dial", Err: errors.New("connect refused")}
	safe := TransportFailure(dial)
	if ReplayForbidden(safe) || !errors.Is(safe, dial) || FactsFromError(safe).SendState != "not_sent" {
		t.Fatal("confirmed pre-send failure cannot retry")
	}
}
