//go:build unix

package s3

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDefaultTempDirIsOSTempDir proves that without WithTempDir the store leaves
// tempDir empty and stages under os.TempDir(). It relies on TMPDIR (honored by
// os.TempDir on unix), so it is unix-guarded.
func TestDefaultTempDirIsOSTempDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	require.Equal(t, dir, os.TempDir(), "test requires os.TempDir to honor TMPDIR")

	staged := make(chan []string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		staged <- stagedFiles(t, dir, "castlet-s3put-")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s, err := New(Config{Endpoint: srv.URL, Bucket: "b", AccessKey: "AK", SecretKey: "SK"})
	require.NoError(t, err)
	assert.Empty(t, s.tempDir, "default store must leave tempDir unset")

	_, err = s.Put(context.Background(), "key", strings.NewReader("hi"))
	require.NoError(t, err)
	require.Len(t, <-staged, 1, "default Put must stage under os.TempDir()")
}
