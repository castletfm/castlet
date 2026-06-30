// Package localfs is the default blob.BlobStore implementation. It stores each
// object as a file under a root directory. Writes are atomic (write-temp +
// rename) so a crash mid-upload never leaves a partial object at its final key.
package localfs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/castletfm/castlet/blob"
)

// Store is a filesystem-backed BlobStore rooted at a directory.
type Store struct {
	root string
}

var _ blob.BlobStore = (*Store)(nil)

// New returns a Store rooted at dir, creating the directory tree if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("localfs: create root %q: %w", dir, err)
	}
	return &Store{root: dir}, nil
}

// path resolves key to a file path, rejecting anything that could escape the
// root. Castlet keys are generated ids, but validating keeps the store safe
// against a careless caller.
func (s *Store) path(key string) (string, error) {
	if key == "" || strings.ContainsAny(key, `/\`) || strings.Contains(key, "..") {
		return "", fmt.Errorf("localfs: invalid key %q", key)
	}
	return filepath.Join(s.root, key), nil
}

func (s *Store) Put(ctx context.Context, key string, r io.Reader) (int64, error) {
	dst, err := s.path(key)
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(s.root, ".upload-*")
	if err != nil {
		return 0, fmt.Errorf("localfs: temp file: %w", err)
	}
	tmpName := tmp.Name()
	// On any failure past this point, don't leave the temp file behind.
	defer os.Remove(tmpName)

	n, err := io.Copy(tmp, r)
	if err != nil {
		tmp.Close()
		return 0, fmt.Errorf("localfs: write %q: %w", key, err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("localfs: close %q: %w", key, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return 0, fmt.Errorf("localfs: commit %q: %w", key, err)
	}
	return n, nil
}

func (s *Store) Get(ctx context.Context, key string) (io.ReadSeekCloser, int64, error) {
	name, err := s.path(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(name)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, blob.ErrNotFound
		}
		return nil, 0, fmt.Errorf("localfs: open %q: %w", key, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("localfs: stat %q: %w", key, err)
	}
	return f, fi.Size(), nil
}

func (s *Store) Delete(ctx context.Context, key string) error {
	name, err := s.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("localfs: delete %q: %w", key, err)
	}
	return nil
}
