package server

import (
	"bytes"
	"net/http"
)

// render executes a page into a buffer first, so a template error becomes a
// clean 500 instead of a half-written response. status is the HTTP status for
// a successful render.
func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, page, title string, data any) {
	vd := &ViewData{
		Site:        s.siteName,
		User:        userFrom(r.Context()),
		Title:       title,
		AllowSignup: s.allowSignup,
		OIDCEnabled: s.authn != nil,
		Data:        data,
	}
	var buf bytes.Buffer
	if err := s.renderer.Render(&buf, page, vd); err != nil {
		s.logger.Error("render", "page", page, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}

// renderError renders the shared error page at the given status.
func (s *Server) renderError(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.render(w, r, status, "error", http.StatusText(status), struct {
		Status  string
		Message string
	}{Status: http.StatusText(status), Message: message})
}

// redirect issues a 303 See Other, the correct status after a successful POST.
func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}
