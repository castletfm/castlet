package server_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/castletfm/castlet/blob/localfs"
	"github.com/castletfm/castlet/internal/session"
	"github.com/castletfm/castlet/queue/dbqueue"
	"github.com/castletfm/castlet/server"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/stretchr/testify/require"
)

// TestShutdownForceClosesStuckConnection verifies the bounded-shutdown
// contract: when a non-idle connection prevents a graceful drain within the
// shutdown timeout, Run must still return (never hang on the serve-error
// channel) and force-close the lingering connection via httpSrv.Close(). A raw
// connection that starts a request but never finishes its headers keeps the
// server non-idle, so Shutdown times out; only the Close fallback then closes
// the socket, which the client observes as EOF.
func TestShutdownForceClosesStuckConnection(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(t.Context()))
	blobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	sess := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv, err := server.New(st, blobs, dbqueue.New(st), sess,
		server.WithAddr("127.0.0.1:0"),
		server.WithShutdownTimeout(100*time.Millisecond),
		server.WithLogger(slog.New(slog.DiscardHandler)),
	)
	require.NoError(t, err)

	ctrl, err := srv.Run(ctx)
	require.NoError(t, err)

	// Open a raw connection and send an incomplete request (no terminating
	// blank line) so the server is stuck reading and never becomes idle.
	conn, err := net.Dial("tcp", ctrl.Addr())
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: test\r\n"))
	require.NoError(t, err)
	// Give the server a moment to accept and start reading the request.
	time.Sleep(150 * time.Millisecond)

	cancel()

	// Run must return well within the shutdown timeout plus the drain grace,
	// never blocking indefinitely on the serve goroutine.
	select {
	case <-ctrl.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after shutdown; it hung")
	}

	// The lingering connection must have been force-closed: a read returns EOF
	// (or a connection error), not a deadline timeout. Without the Close
	// fallback the socket would stay open and this read would time out.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err)
	var nerr net.Error
	if errors.As(err, &nerr) {
		require.False(t, nerr.Timeout(), "connection should be force-closed, not left to time out")
	}
}
