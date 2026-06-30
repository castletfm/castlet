package server

import (
	"net/http"

	"github.com/castletfm/castlet/web"
)

// reservedSlugs are top-level path segments the router owns; a channel slug may
// not collide with them.
var reservedSlugs = map[string]struct{}{
	"admin":  {},
	"login":  {},
	"logout": {},
	"media":  {},
	"static": {},
}

func isReservedSlug(s string) bool {
	_, ok := reservedSlugs[s]
	return ok
}

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

	// Admin (auth required).
	mux.HandleFunc("GET /admin/{$}", s.requireAuth(s.handleAdminDashboard))
	mux.HandleFunc("GET /admin/channels/new", s.requireAuth(s.handleChannelNew))
	mux.HandleFunc("POST /admin/channels", s.requireAuth(s.handleChannelCreate))
	mux.HandleFunc("GET /admin/channels/{id}/edit", s.requireAuth(s.handleChannelEdit))
	mux.HandleFunc("POST /admin/channels/{id}", s.requireAuth(s.handleChannelUpdate))
	mux.HandleFunc("GET /admin/channels/{id}/episodes", s.requireAuth(s.handleEpisodeList))
	mux.HandleFunc("GET /admin/channels/{id}/episodes/new", s.requireAuth(s.handleEpisodeNew))
	mux.HandleFunc("POST /admin/channels/{id}/episodes", s.requireAuth(s.handleEpisodeCreate))
	mux.HandleFunc("POST /admin/episodes/{id}/publish", s.requireAuth(s.handleEpisodePublish))
	mux.HandleFunc("POST /admin/episodes/{id}/unpublish", s.requireAuth(s.handleEpisodeUnpublish))
	mux.HandleFunc("POST /admin/episodes/{id}/delete", s.requireAuth(s.handleEpisodeDelete))

	// Public pages live under top-level, user-chosen slugs (/{channel}/,
	// /{channel}/{episode}/, /{channel}/feed.xml). A single-segment wildcard
	// would conflict with the literal subtrees above (/static/, /admin/), so
	// instead this least-specific catch-all dispatches them internally; every
	// literal route registered above is more specific and still wins.
	mux.HandleFunc("GET /{path...}", s.handlePublic)

	return s.logRequests(s.loadUser(mux))
}
