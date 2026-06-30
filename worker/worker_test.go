package worker_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/castletfm/castlet/blob/localfs"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/queue/dbqueue"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/castletfm/castlet/transcribe"
	"github.com/castletfm/castlet/transcribe/null"
	"github.com/castletfm/castlet/worker"
	"github.com/stretchr/testify/require"
)

// fakeTranscriber returns a fixed two-segment transcript.
type fakeTranscriber struct{}

func (fakeTranscriber) Transcribe(ctx context.Context, in transcribe.Input) (*transcribe.Result, error) {
	return &transcribe.Result{Language: "en", Segments: []transcribe.Segment{
		{StartSecs: 0, EndSecs: 1, Text: "hello"},
		{StartSecs: 1, EndSecs: 2, Text: "world"},
	}}, nil
}

func setup(t *testing.T) (*sqlite.Store, *localfs.Store, *dbqueue.Queue) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "w.db"))
	require.NoError(t, err)
	require.NoError(t, st.Migrate(t.Context()))
	t.Cleanup(func() { st.Close() })
	blobs, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	return st, blobs, dbqueue.New(st)
}

func seedEpisode(t *testing.T, st *sqlite.Store, blobs *localfs.Store, q *dbqueue.Queue) string {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, st.CreateUser(ctx, &model.User{ID: "u1", Email: "u@x.y", DisplayName: "U", CreatedAt: time.Now()}))
	require.NoError(t, st.CreateChannel(ctx, &model.Channel{ID: "c1", UserID: "u1", Title: "S",
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	_, err := blobs.Put(ctx, "mk1", strings.NewReader("fake audio bytes"))
	require.NoError(t, err)
	require.NoError(t, st.CreateEpisode(ctx, &model.Episode{ID: "e1", ChannelID: "c1", Title: "E",
		MediaKey: "mk1", MediaMIME: "audio/mpeg", MediaKind: model.MediaAudio,
		Status: model.EpisodeDraft, TranscriptStatus: model.TranscriptPending,
		CreatedAt: time.Now(), UpdatedAt: time.Now()}))
	require.NoError(t, q.Enqueue(ctx, model.JobTranscribe, model.TranscribePayload{EpisodeID: "e1"}))
	return "e1"
}

// waitStatus polls until the episode reaches want or the deadline passes.
func waitStatus(t *testing.T, st *sqlite.Store, id string, want model.TranscriptStatus) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		ep, err := st.EpisodeByID(t.Context(), id)
		require.NoError(t, err)
		if ep.TranscriptStatus == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("episode %s did not reach status %q in time", id, want)
}

func TestWorkerTranscribes(t *testing.T) {
	st, blobs, q := setup(t)
	id := seedEpisode(t, st, blobs, q)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctrl, err := worker.New(st, blobs, q, fakeTranscriber{}, worker.WithPollInterval(10*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	waitStatus(t, st, id, model.TranscriptDone)

	tr, err := st.TranscriptByEpisode(t.Context(), id)
	require.NoError(t, err)
	require.Len(t, tr.Segments, 2)
	require.Equal(t, "world", tr.Segments[1].Text)

	cancel()
	require.NoError(t, ctrl.Wait())
}

func TestWorkerNullSettlesToNone(t *testing.T) {
	st, blobs, q := setup(t)
	id := seedEpisode(t, st, blobs, q)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err := worker.New(st, blobs, q, null.New(), worker.WithPollInterval(10*time.Millisecond)).Run(ctx)
	require.NoError(t, err)

	// null transcriber reports unsupported -> transcript status becomes "none"
	waitStatus(t, st, id, model.TranscriptNone)

	_, err = st.TranscriptByEpisode(t.Context(), id)
	require.Error(t, err, "no transcript should be saved")
}
