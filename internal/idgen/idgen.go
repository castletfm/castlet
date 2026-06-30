// Package idgen generates opaque, URL-safe, collision-resistant identifiers.
package idgen

import (
	"crypto/rand"
	"encoding/base32"
)

// crockford is Crockford base32: case-insensitive, no padding, and omits the
// ambiguous letters I, L, O, U. The result is compact and URL-safe.
var crockford = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// New returns a fresh 128-bit random identifier encoded as 26 Crockford
// base32 characters. It panics only if the system CSPRNG fails, which is
// unrecoverable.
func New() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("idgen: crypto/rand failed: " + err.Error())
	}
	return crockford.EncodeToString(b[:])
}
