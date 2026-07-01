package server

import (
	"net/http"
	"testing"
	"time"
)

// TestUploadReadTimeout guards the read-deadline derivation: at the default
// 512 MiB cap the window must be generously larger than both the global
// readTimeout and the floor, otherwise extending the deadline would not let a
// large upload over a slow link outlast the global body-drip cap. A tiny cap
// must fall back to the floor.
func TestUploadReadTimeout(t *testing.T) {
	s := &Server{maxUploadBytes: 512 << 20} // default cap
	got := s.uploadReadTimeout()
	if got <= readTimeout {
		t.Fatalf("uploadReadTimeout (%s) must exceed readTimeout (%s)", got, readTimeout)
	}
	if got <= uploadTimeoutFloor {
		t.Fatalf("uploadReadTimeout (%s) must exceed the floor (%s)", got, uploadTimeoutFloor)
	}
	if want := time.Hour; got < want {
		t.Fatalf("uploadReadTimeout (%s) for the default cap must be generous (>= %s)", got, want)
	}

	small := &Server{maxUploadBytes: 1 << 10} // 1 KiB, well below the floor rate
	if got := small.uploadReadTimeout(); got != uploadTimeoutFloor {
		t.Fatalf("uploadReadTimeout for a tiny cap = %s, want floor %s", got, uploadTimeoutFloor)
	}

	// A cap that is not an exact multiple of minUploadRate must round the window
	// UP: floor division would leave the trailing bytes without enough time, so
	// a client sending at exactly minUploadRate could be cut off. Use a cap above
	// the floor so the rate-derived value (not the floor) is exercised.
	const notMultiple = 400*minUploadRate + 1 // ~50 MiB + 1 byte, above the floor rate
	big := &Server{maxUploadBytes: notMultiple}
	got = big.uploadReadTimeout()
	if floored := time.Duration(notMultiple/minUploadRate) * time.Second; got <= floored {
		t.Fatalf("uploadReadTimeout (%s) must round up past the floored value (%s)", got, floored)
	}
	if need := time.Duration(minUploadRate) * got / time.Second; need < notMultiple {
		t.Fatalf("uploadReadTimeout (%s) delivers only %d bytes at minUploadRate, want >= %d", got, need, notMultiple)
	}
}

// deadlineWriter is a fake deadline-capable ResponseWriter recording the last
// read deadline it was asked to set.
type deadlineWriter struct {
	http.ResponseWriter
	deadline time.Time
	called   bool
}

func (w *deadlineWriter) SetReadDeadline(t time.Time) error {
	w.deadline = t
	w.called = true
	return nil
}

// TestStatusWriterUnwrapReachesDeadline guards the upload read-deadline: because
// logRequests wraps the ResponseWriter in a statusWriter, SetReadDeadline only
// takes effect if statusWriter implements Unwrap so http.ResponseController can
// traverse to the deadline-capable writer. Without Unwrap the controller returns
// http.ErrNotSupported and the deadline silently no-ops.
func TestStatusWriterUnwrapReachesDeadline(t *testing.T) {
	fake := &deadlineWriter{}
	sw := &statusWriter{ResponseWriter: fake, status: http.StatusOK}

	want := time.Unix(1234567890, 0)
	if err := http.NewResponseController(sw).SetReadDeadline(want); err != nil {
		t.Fatalf("SetReadDeadline through statusWriter: %v", err)
	}
	if !fake.called {
		t.Fatal("SetReadDeadline did not reach the underlying writer (statusWriter.Unwrap missing?)")
	}
	if !fake.deadline.Equal(want) {
		t.Fatalf("deadline = %s, want %s", fake.deadline, want)
	}
}
