package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/castletfm/castlet/blob"
	"github.com/castletfm/castlet/blob/localfs"
	"github.com/castletfm/castlet/internal/session"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue/dbqueue"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/stretchr/testify/require"
)

// fakeWriteDeadlineWriter records every write deadline it is asked to set. It
// embeds a recorder so bytes written through it are observable.
type fakeWriteDeadlineWriter struct {
	http.ResponseWriter
	deadlines []time.Time
}

func (w *fakeWriteDeadlineWriter) SetWriteDeadline(t time.Time) error {
	w.deadlines = append(w.deadlines, t)
	return nil
}

// TestStreamWithIdleDeadlineRefreshesPerWrite proves the streamed-media write
// wrapper arms an idle write deadline up front and pushes it out on every write
// relative to the current clock, so a large but progressing download keeps
// resetting its deadline (never truncated) while the bytes still reach the
// underlying writer.
func TestStreamWithIdleDeadlineRefreshesPerWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	base := &fakeWriteDeadlineWriter{ResponseWriter: rec}
	clock := time.Unix(1000, 0)
	s := &Server{mediaWriteIdle: 30 * time.Second, now: func() time.Time { return clock }}

	w := s.streamWithIdleDeadline(base)
	require.IsType(t, &idleDeadlineWriter{}, w, "a deadline-capable writer must be wrapped")
	require.Len(t, base.deadlines, 1, "the deadline must be armed once up front")
	require.Equal(t, clock.Add(30*time.Second), base.deadlines[0])

	clock = clock.Add(10 * time.Second)
	_, err := w.Write([]byte("hello"))
	require.NoError(t, err)
	require.Len(t, base.deadlines, 2)
	require.Equal(t, clock.Add(30*time.Second), base.deadlines[1], "each write refreshes off the current clock")

	clock = clock.Add(10 * time.Second)
	_, err = w.Write([]byte("world"))
	require.NoError(t, err)
	require.Len(t, base.deadlines, 3)
	require.Equal(t, clock.Add(30*time.Second), base.deadlines[2])

	require.Equal(t, "helloworld", rec.Body.String(), "bytes must still reach the underlying writer")
}

// TestStreamWithIdleDeadlineDegradesWithoutSupport proves that when the writer's
// connection cannot set a write deadline (http.ErrNotSupported — e.g. an
// httptest recorder), the stream is served unwrapped rather than failing.
func TestStreamWithIdleDeadlineDegradesWithoutSupport(t *testing.T) {
	rec := httptest.NewRecorder() // no SetWriteDeadline support
	s := &Server{mediaWriteIdle: 30 * time.Second, now: time.Now}

	w := s.streamWithIdleDeadline(rec)
	_, wrapped := w.(*idleDeadlineWriter)
	require.False(t, wrapped, "a writer without deadline support must not be wrapped")

	_, err := w.Write([]byte("ok"))
	require.NoError(t, err)
	require.Equal(t, "ok", rec.Body.String())
}

// closeSignalReader wraps a blob reader and closes done exactly once when the
// server closes it. handleMedia does `defer rc.Close()`, so done firing proves
// the streaming handler returned and released the open blob reader.
type closeSignalReader struct {
	io.ReadSeekCloser
	done   chan struct{}
	closed bool
}

func (r *closeSignalReader) Close() error {
	if !r.closed {
		r.closed = true
		close(r.done)
	}
	return r.ReadSeekCloser.Close()
}

// closeSignalBlobs wraps a BlobStore so the reader returned by Get signals on
// Close.
type closeSignalBlobs struct {
	blob.BlobStore
	done chan struct{}
}

func (b *closeSignalBlobs) Get(ctx context.Context, key string) (io.ReadSeekCloser, int64, error) {
	rc, n, err := b.BlobStore.Get(ctx, key)
	if err != nil {
		return rc, n, err
	}
	return &closeSignalReader{ReadSeekCloser: rc, done: b.done}, n, err
}

// TestMediaStreamStalledReaderReleased proves the end-to-end defense: an
// unauthenticated client that starts a /media download and then stops reading no
// longer pins the streaming handler forever. Once the idle write deadline
// elapses, ServeContent's blocked write fails, the handler returns, and its
// `defer rc.Close()` fires — observed here via a Close signal — instead of the
// goroutine + open blob reader being held indefinitely. Without the deadline the
// reader would never be closed and this test would time out.
//
// The body is larger than the socket buffers (the client also shrinks its
// receive buffer) so the non-reading client makes the server's write block and
// trip the deadline rather than the whole body being buffered and the download
// simply completing.
func TestMediaStreamStalledReaderReleased(t *testing.T) {
	ctx := t.Context()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "t.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(ctx))
	fs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	blobs := &closeSignalBlobs{BlobStore: fs, done: make(chan struct{})}
	sess := session.NewManager([]byte("0123456789abcdef0123456789abcdef"))

	const key = "bigmedia"
	body := bytes.Repeat([]byte("A"), 32<<20) // 32 MiB, past the socket buffers
	_, err = fs.Put(ctx, key, bytes.NewReader(body))
	require.NoError(t, err)
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "a@b.c",
		DisplayName: "A", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1",
		Title: "S", Language: "en", CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, st.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1",
		Title: "T", MediaKey: key, MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		MediaBytes: int64(len(body)), Status: model.EpisodePublished,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))

	srv, err := New(st, blobs, dbqueue.New(st), sess, WithAddr("127.0.0.1:0"))
	require.NoError(t, err)
	const idle = 300 * time.Millisecond
	srv.mediaWriteIdle = idle
	ctrl, err := srv.Run(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { <-ctrl.Done() })

	conn, err := net.Dial("tcp", ctrl.Addr())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	// Shrink the client receive buffer so the server's send buffer fills quickly,
	// making the non-reading client block the server's write (and trip the idle
	// deadline) well before the body could be fully buffered.
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(4096))

	_, err = fmt.Fprintf(conn, "GET /media/%s HTTP/1.1\r\nHost: x\r\n\r\n", key)
	require.NoError(t, err)

	// Confirm the stream started, then stall: read nothing further so the server
	// write blocks on the full socket buffer.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	head := make([]byte, 256)
	n, err := conn.Read(head)
	require.NoError(t, err)
	require.Greater(t, n, 0)
	require.Contains(t, string(head[:n]), "200 OK")

	// The idle write deadline must fire, unblock ServeContent, and release the
	// blob reader well within a generous bound. Without the fix Close never fires.
	select {
	case <-blobs.done:
	case <-time.After(5 * time.Second):
		t.Fatal("stalled reader pinned the streaming handler: blob reader was not released within 5s")
	}
}
