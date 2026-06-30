package localfs_test

import (
	"io"
	"strings"
	"testing"

	"github.com/castletfm/castlet/blob"
	"github.com/castletfm/castlet/blob/localfs"
	"github.com/stretchr/testify/require"
)

func TestPutGetDelete(t *testing.T) {
	s, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	ctx := t.Context()

	n, err := s.Put(ctx, "abc", strings.NewReader("hello world"))
	require.NoError(t, err)
	require.EqualValues(t, 11, n)

	r, size, err := s.Get(ctx, "abc")
	require.NoError(t, err)
	require.EqualValues(t, 11, size)
	data, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Equal(t, "hello world", string(data))

	// overwrite
	_, err = s.Put(ctx, "abc", strings.NewReader("bye"))
	require.NoError(t, err)
	r, _, err = s.Get(ctx, "abc")
	require.NoError(t, err)
	data, _ = io.ReadAll(r)
	r.Close()
	require.Equal(t, "bye", string(data))

	require.NoError(t, s.Delete(ctx, "abc"))
	_, _, err = s.Get(ctx, "abc")
	require.ErrorIs(t, err, blob.ErrNotFound)

	// deleting a missing key is not an error
	require.NoError(t, s.Delete(ctx, "abc"))
}

func TestRejectsUnsafeKeys(t *testing.T) {
	s, err := localfs.New(t.TempDir())
	require.NoError(t, err)
	ctx := t.Context()

	for _, key := range []string{"", "a/b", `a\b`, "../escape", "x..y"} {
		_, err := s.Put(ctx, key, strings.NewReader("x"))
		require.Error(t, err, "key %q must be rejected", key)
	}
}
