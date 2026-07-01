package server

import (
	"errors"
	"mime"
	"net/http"
)

// maxSmallFormBytes caps the request body accepted by the non-upload POST
// handlers (login, signup, logout, and the admin channel/episode forms). Those
// forms carry only a handful of short fields, so 64 KiB is far more than any
// legitimate submission needs. The media upload handler (handleEpisodeCreate) is
// deliberately exempt: it streams a large multipart body under its own,
// size-derived cap and read deadline.
const maxSmallFormBytes = 64 << 10 // 64 KiB

// errNotURLEncodedForm is returned by parseSmallForm when a non-upload POST
// arrives with anything other than an application/x-www-form-urlencoded body. It
// lets the caller bail out (the response has already been written) without
// needing to inspect the concrete error.
var errNotURLEncodedForm = errors.New("server: form must be application/x-www-form-urlencoded")

// parseSmallForm caps and parses the body of a non-upload form POST so the
// caller can safely read fields with r.FormValue afterwards. On any failure it
// writes the error response and returns a non-nil error; the caller must return
// immediately.
//
// It closes two gaps that a bare r.FormValue / r.ParseForm leaves open on these
// handlers:
//
//   - A multipart/form-data body is spooled to a temp file (os.TempDir) by
//     ParseMultipartForm regardless of size, so a plain r.FormValue on a
//     small-form handler lets an attacker write an arbitrarily large file to
//     disk. The CSRF middleware deliberately never parses multipart — its token
//     can ride in the query string — so a POST carrying the token in the query
//     and a huge multipart body otherwise slips straight through the CSRF check
//     to FormValue. We therefore reject anything that is not
//     application/x-www-form-urlencoded with 415 before the body is touched.
//
//   - Even a urlencoded body is otherwise only bounded by net/http's 10 MiB
//     default. Wrapping r.Body in a 64 KiB MaxBytesReader caps it tightly. The
//     reader is handed the UNWRAPPED ResponseWriter (via underlying, like the
//     upload handler) so its oversized-body hook still fires — MaxBytesReader
//     does not follow Unwrap — and so it composes with any read deadline set on
//     the underlying connection.
func (s *Server) parseSmallForm(w http.ResponseWriter, r *http.Request) error {
	// Require application/x-www-form-urlencoded before reading a single byte. A
	// missing, malformed, or multipart Content-Type is rejected here so no
	// oversized/multipart body is ever spooled by a later FormValue call.
	ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || ct != "application/x-www-form-urlencoded" {
		s.renderError(w, r, http.StatusUnsupportedMediaType,
			"This form must be submitted as application/x-www-form-urlencoded.")
		return errNotURLEncodedForm
	}

	// Cap the body before parsing. Passing the unwrapped ResponseWriter lets
	// MaxBytesReader fire net/http's oversized-body hook (flag the connection to
	// close, skip draining) instead of silently no-opping through the wrapper.
	r.Body = http.MaxBytesReader(underlying(w), r.Body, maxSmallFormBytes)

	if err := r.ParseForm(); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			s.renderError(w, r, http.StatusRequestEntityTooLarge, "The submitted form is too large.")
			return err
		}
		s.renderError(w, r, http.StatusBadRequest, "The form could not be read.")
		return err
	}
	return nil
}
