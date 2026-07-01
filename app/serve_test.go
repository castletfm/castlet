package app

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/config"
	"github.com/stretchr/testify/require"
)

// newTestApp builds an App with the default backends against a throwaway data
// dir, listening on addr. It is enough to exercise the Serve lifecycle without
// any external services (null transcriber, localfs blobs, sqlite).
func newTestApp(t *testing.T, addr string) *App {
	t.Helper()
	cfg := &config.Config{
		Addr:       addr,
		BaseURL:    "http://" + addr,
		DataDir:    t.TempDir(),
		SiteName:   "Castlet Test",
		SessionKey: []byte(strings.Repeat("k", 32)),
	}
	a, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, a.Migrate(context.Background()))
	return a
}

// TestServeFailsFastOnBoundAddr is the regression guard for the startup-ordering
// fix (ADV-005). The HTTP listener is bound BEFORE the worker goroutine starts,
// so a bind failure (address already in use) must return an error promptly with
// no worker left running to race the caller's `defer App.Close()` store close.
//
// The test occupies a port, points the App at it, and asserts Serve returns an
// error quickly (rather than hanging or racing). Because the bind happens before
// the worker starts, a failed bind means the worker never ran at all — there is
// nothing to leak.
func TestServeFailsFastOnBoundAddr(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	a := newTestApp(t, ln.Addr().String())
	defer a.Close()

	done := make(chan error, 1)
	go func() { done <- a.Serve(context.Background()) }()

	select {
	case err := <-done:
		require.Error(t, err, "Serve must fail when the listen address is already bound")
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after a failed bind; it may have leaked the worker or hung")
	}
}

// TestServeCleanShutdownStopsBoth guards the normal path: cancelling the context
// must stop BOTH the server and the worker and let Serve return without error.
// It also proves Serve blocks on both controllers' Done() before returning, so
// no background goroutine outlives Serve to race App.Close.
func TestServeCleanShutdownStopsBoth(t *testing.T) {
	a := newTestApp(t, "127.0.0.1:0")
	defer a.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()

	// Give Serve time to bind the listener and start the worker before asking
	// it to shut down.
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		require.NoError(t, err, "a ctx-cancelled shutdown is clean")
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after context cancellation")
	}
}
