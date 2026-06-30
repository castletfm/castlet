// Package blob defines the opaque binary object store for Castlet: episode
// audio and channel cover art. It is deliberately minimal so the default
// local-filesystem implementation stays trivial and an S3/GCS implementation
// is a thin adapter. Content type, size, and ownership live in the metadata
// Store, not here.
package blob

import (
	"context"
	"errors"
	"io"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("blob: not found")

// BlobStore stores and retrieves binary objects by key. Keys are opaque
// strings chosen by the caller (Castlet uses generated ids). Implementations
// must be safe for concurrent use.
type BlobStore interface {
	// Put stores the full contents of r under key, overwriting any existing
	// object, and returns the number of bytes written.
	Put(ctx context.Context, key string, r io.Reader) (int64, error)
	// Get opens the object for reading. The returned reader is seekable so
	// callers can serve HTTP range requests, and must be closed by the caller.
	// It returns ErrNotFound if the key does not exist.
	Get(ctx context.Context, key string) (io.ReadSeekCloser, int64, error)
	// Delete removes the object. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
}

// DirectURL is an optional capability: a BlobStore that can hand a client a
// URL to fetch an object directly (e.g. a presigned object-store URL), so the
// server can redirect instead of streaming the bytes itself. The server checks
// for this once at startup (a type assertion) and serves accordingly for the
// life of the process; a store that does not implement it is always streamed.
type DirectURL interface {
	// URL returns a fetch URL for key. contentType is the Content-Type the
	// response should carry (the server knows it from the episode); an
	// implementation may encode it into the URL.
	URL(ctx context.Context, key, contentType string) (string, error)
}
