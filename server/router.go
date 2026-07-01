package server

import (
	"net/http"

	"github.com/castletfm/castlet/web"
)

// handler builds the full middleware-wrapped HTTP handler.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	// Static assets (embedded). The {path...} wildcard (rather than a bare
	// /static/ subtree) keeps a literal first segment, so it stays
	// unambiguous against the /{channel}/ wildcard routes below. The FS is
	// rooted so /static/app.css maps to static/app.css inside the embed.
	mux.Handle("GET /static/{path...}", http.FileServerFS(web.Static))

	// Media bytes.
	mux.HandleFunc("GET /media/{key}", s.handleMedia)

	// Auth.
	mux.HandleFunc("GET /login", s.handleLoginForm)
	mux.HandleFunc("POST /login", s.handleLogin)
	mux.HandleFunc("POST /logout", s.handleLogout)
	mux.HandleFunc("GET /signup", s.handleSignupForm)
	mux.HandleFunc("POST /signup", s.handleSignup)
	mux.HandleFunc("GET /auth/oidc/login", s.handleOIDCLogin)
	mux.HandleFunc("GET /auth/oidc/callback", s.handleOIDCCallback)

	// Admin (auth required).
	mux.HandleFunc("GET /admin/{$}", s.requireAuth(s.handleAdminDashboard))
	mux.HandleFunc("GET /admin/upload", s.requireAuth(s.handleUpload))
	mux.HandleFunc("GET /admin/channels/new", s.requireAuth(s.handleChannelNew))
	mux.HandleFunc("POST /admin/channels", s.requireAuth(s.handleChannelCreate))
	mux.HandleFunc("GET /admin/channels/{id}/edit", s.requireAuth(s.handleChannelEdit))
	mux.HandleFunc("POST /admin/channels/{id}", s.requireAuth(s.handleChannelUpdate))
	mux.HandleFunc("GET /admin/channels/{id}/episodes", s.requireAuth(s.handleEpisodeList))
	mux.HandleFunc("GET /admin/channels/{id}/episodes/new", s.requireAuth(s.handleEpisodeNew))
	mux.HandleFunc("POST /admin/channels/{id}/episodes", s.requireAuth(s.handleEpisodeCreate))
	mux.HandleFunc("GET /admin/episodes/{id}/edit", s.requireAuth(s.handleEpisodeEdit))
	mux.HandleFunc("POST /admin/episodes/{id}", s.requireAuth(s.handleEpisodeUpdate))
	mux.HandleFunc("POST /admin/episodes/{id}/publish", s.requireAuth(s.handleEpisodePublish))
	mux.HandleFunc("POST /admin/episodes/{id}/unpublish", s.requireAuth(s.handleEpisodeUnpublish))
	mux.HandleFunc("POST /admin/episodes/{id}/move", s.requireAuth(s.handleEpisodeMove))
	mux.HandleFunc("POST /admin/episodes/{id}/transcribe", s.requireAuth(s.handleEpisodeTranscribe))
	mux.HandleFunc("GET /admin/episodes/{id}/status", s.requireAuth(s.handleEpisodeStatus))
	mux.HandleFunc("POST /admin/episodes/{id}/delete", s.requireAuth(s.handleEpisodeDelete))

	// Public pages, addressed by opaque id (no user-chosen slugs): channels
	// under /c/{id}/ with their feed, episodes under /e/{id}/. The literal /c/
	// and /e/ prefixes keep them unambiguous against /admin/, /static/, etc.
	mux.HandleFunc("GET /{$}", s.handleLanding)
	mux.HandleFunc("GET /c/{id}/{$}", s.handleChannel)
	mux.HandleFunc("GET /c/{id}/feed.xml", s.handleFeed)
	mux.HandleFunc("GET /e/{id}/{$}", s.handleEpisode)

	// logRequests is outermost so a recovered panic still produces the normal
	// completion line (with the 500 status); securityHeaders then sets baseline
	// headers on every response (including error pages) before recoverPanic
	// wraps loadUser and the handlers so their panics become a logged 500.
	return s.logRequests(s.securityHeaders(s.recoverPanic(s.loadUser(mux))))
}
