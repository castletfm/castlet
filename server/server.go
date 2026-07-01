// Package server is Castlet's HTTP front end: public channel/episode pages, an
// RSS feed, media delivery, cookie-based login, and the admin area. It follows
// the house Run/Controller lifecycle: Run binds the listener synchronously and
// returns a Controller; cancelling the context passed to Run gracefully shuts
// the server down. A per-request recover() middleware turns a panic in any
// handler into a logged 500 (and re-panics http.ErrAbortHandler), so one bad
// request cannot take the process down; non-request goroutines still follow
// the existing "let it crash" policy and rely on the operator's restart policy.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/castletfm/castlet/auth"
	"github.com/castletfm/castlet/blob"
	"github.com/castletfm/castlet/internal/metrics"
	"github.com/castletfm/castlet/internal/session"
	"github.com/castletfm/castlet/queue"
	"github.com/castletfm/castlet/store"
	"github.com/lestrrat-go/option/v3"
)

// ErrServerClosed is recorded on the Controller when the server stops because
// its context was cancelled, distinguishing a clean shutdown from a crash.
var ErrServerClosed = errors.New("server: closed")

// readTimeout bounds the whole request read (headers + body) for normal routes
// so a client cannot send headers and then drip the body indefinitely
// (slowloris-on-body). It is generous for small/bodyless requests; the upload
// handler extends its own read deadline (Server.uploadReadTimeout) so large
// media uploads over slow links are not cut off by this global cap.
const readTimeout = 30 * time.Second

// shutdownDrainGrace bounds how long Run waits for the Serve goroutine to
// return after Shutdown/Close before giving up on it, so Run always returns.
const shutdownDrainGrace = 2 * time.Second

// mediaWriteIdle is the default idle window applied to a streamed /media
// response: each successful write refreshes the connection's write deadline by
// this much, so a large-but-progressing download is never cut off while a
// reader that stalls for longer than the window is dropped. This bounds a
// slow-read DoS on the streaming (localfs / non-DirectURL) media path, which
// would otherwise pin a goroutine, connection, and open blob reader
// indefinitely because http.Server.WriteTimeout is deliberately left unset. The
// DirectURL path 302-redirects and streams no bytes, so it needs no deadline.
const mediaWriteIdle = 60 * time.Second

// Server serves the Castlet web application. The receiver holds only validated
// configuration and is safe to Run more than once.
type Server struct {
	store store.Store
	blobs blob.BlobStore
	// directBlobs is set (once, at construction) when the blob store can serve
	// objects directly via a URL; then the media handler redirects instead of
	// streaming. nil means always stream.
	directBlobs blob.DirectURL
	queue       queue.JobQueue
	sessions    *session.Manager
	renderer    Renderer
	authn       auth.Authenticator // nil when OIDC is disabled
	metrics     *metrics.Registry
	// loginLimiter throttles password-login brute force per client IP. OIDC SSO
	// and GET routes are unaffected.
	loginLimiter *loginLimiter

	addr            string
	baseURL         string
	siteName        string
	allowSignup     bool
	allowedDomains  []string // email domains permitted to sign in via OIDC; empty allows any
	maxUploadBytes  int64
	shutdownTimeout time.Duration
	// mediaWriteIdle is the idle window that bounds each streamed /media write;
	// defaulted to the mediaWriteIdle constant in New. Kept as a field so tests
	// can shrink it without waiting on the production window.
	mediaWriteIdle time.Duration
	logger         *slog.Logger
	now            func() time.Time
}

// Option configures New.
type Option = option.Interface

type (
	identAddr            struct{}
	identBaseURL         struct{}
	identSiteName        struct{}
	identMaxUploadBytes  struct{}
	identLogger          struct{}
	identRenderer        struct{}
	identAllowSignup     struct{}
	identAuthenticator   struct{}
	identAllowedDomains  struct{}
	identMetrics         struct{}
	identShutdownTimeout struct{}
)

// WithAddr sets the listen address (default ":8080").
func WithAddr(addr string) Option { return option.New(identAddr{}, addr) }

// WithBaseURL sets the absolute site root used for feed and enclosure URLs
// (default "http://localhost:8080").
func WithBaseURL(u string) Option { return option.New(identBaseURL{}, u) }

// WithSiteName sets the site name shown in the header and titles
// (default "Castlet").
func WithSiteName(name string) Option { return option.New(identSiteName{}, name) }

// WithMaxUploadBytes caps the size of an episode audio upload
// (default 512 MiB).
func WithMaxUploadBytes(n int64) Option { return option.New(identMaxUploadBytes{}, n) }

// WithLogger sets the structured logger (default slog.Default()).
func WithLogger(l *slog.Logger) Option { return option.New(identLogger{}, l) }

// WithRenderer overrides the view renderer, the seam for an alternative
// frontend. The default renders the embedded html/template pages.
func WithRenderer(r Renderer) Option { return option.New(identRenderer{}, r) }

// WithAllowSignup enables the self-service local sign-up path (default false).
func WithAllowSignup(allow bool) Option { return option.New(identAllowSignup{}, allow) }

// WithAuthenticator enables OIDC single sign-on using the given authenticator.
// When unset, OIDC routes are disabled and the SSO button is hidden.
func WithAuthenticator(a auth.Authenticator) Option { return option.New(identAuthenticator{}, a) }

// WithAllowedDomains restricts OIDC sign-in (linking and just-in-time
// provisioning) to the given email domains. An empty list allows any domain.
func WithAllowedDomains(domains []string) Option { return option.New(identAllowedDomains{}, domains) }

// WithMetrics sets the metrics registry the server records HTTP metrics into and
// serves at /metrics. Share one registry with the worker so a single scrape
// covers both. When unset, the server creates a private registry.
func WithMetrics(r *metrics.Registry) Option { return option.New(identMetrics{}, r) }

// WithShutdownTimeout bounds how long a context-driven shutdown waits for
// in-flight requests to drain before remaining connections are force-closed
// (default 10s).
func WithShutdownTimeout(d time.Duration) Option { return option.New(identShutdownTimeout{}, d) }

// New constructs a Server from its dependencies. It returns an error only if
// the default renderer fails to parse its templates.
func New(st store.Store, blobs blob.BlobStore, q queue.JobQueue, sessions *session.Manager, options ...Option) (*Server, error) {
	direct, _ := blobs.(blob.DirectURL) // nil unless the backend serves directly
	s := &Server{
		store:           st,
		blobs:           blobs,
		directBlobs:     direct,
		queue:           q,
		sessions:        sessions,
		addr:            ":8080",
		baseURL:         "http://localhost:8080",
		siteName:        "Castlet",
		maxUploadBytes:  512 << 20,
		shutdownTimeout: 10 * time.Second,
		mediaWriteIdle:  mediaWriteIdle,
		logger:          slog.Default(),
		now:             time.Now,
		loginLimiter:    newLoginLimiter(loginRateLimitMax, loginRateLimitWindow, loginLimiterMaxEntries),
	}
	for _, o := range options {
		switch o.Ident().(type) {
		case identAddr:
			s.addr = option.MustGet[string](o)
		case identBaseURL:
			s.baseURL = option.MustGet[string](o)
		case identSiteName:
			s.siteName = option.MustGet[string](o)
		case identMaxUploadBytes:
			s.maxUploadBytes = option.MustGet[int64](o)
		case identLogger:
			s.logger = option.MustGet[*slog.Logger](o)
		case identRenderer:
			s.renderer = option.MustGet[Renderer](o)
		case identAllowSignup:
			s.allowSignup = option.MustGet[bool](o)
		case identAuthenticator:
			s.authn = option.MustGet[auth.Authenticator](o)
		case identAllowedDomains:
			s.allowedDomains = option.MustGet[[]string](o)
		case identMetrics:
			s.metrics = option.MustGet[*metrics.Registry](o)
		case identShutdownTimeout:
			s.shutdownTimeout = option.MustGet[time.Duration](o)
		}
	}
	if s.metrics == nil {
		s.metrics = metrics.New()
	}
	s.registerMetrics()
	if s.renderer == nil {
		r, err := newTemplateRenderer()
		if err != nil {
			return nil, fmt.Errorf("server: %w", err)
		}
		s.renderer = r
	}
	return s, nil
}

// Controller is the handle to a running Server.
type Controller struct {
	done chan struct{}
	err  atomic.Pointer[error]
	addr string
}

// Done is closed when the server goroutine has fully exited.
func (c *Controller) Done() <-chan struct{} { return c.done }

// Addr is the actual bound address (useful when the configured port was 0).
func (c *Controller) Addr() string { return c.addr }

// Err returns the terminal error, or nil for a clean context-driven shutdown.
func (c *Controller) Err() error {
	if p := c.err.Load(); p != nil && !errors.Is(*p, ErrServerClosed) && !errors.Is(*p, http.ErrServerClosed) {
		return *p
	}
	return nil
}

// Wait blocks until the server exits and returns Err.
func (c *Controller) Wait() error { <-c.done; return c.Err() }

// Run binds the listener and starts serving, returning immediately. A bind
// failure is returned synchronously; runtime errors surface via the Controller.
func (s *Server) Run(ctx context.Context) (*Controller, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.addr)
	if err != nil {
		return nil, fmt.Errorf("server: listen %q: %w", s.addr, err)
	}
	httpSrv := &http.Server{
		Handler:           s.handler(),
		ReadHeaderTimeout: 15 * time.Second,
		ReadTimeout:       readTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
		// WriteTimeout is deliberately left unset: /media streams large media
		// files, and a global write deadline would truncate long downloads.
	}
	ctrl := &Controller{done: make(chan struct{}), addr: ln.Addr().String()}
	go func() {
		defer close(ctrl.done)
		serveErr := make(chan error, 1)
		go func() { serveErr <- httpSrv.Serve(ln) }()
		select {
		case <-ctx.Done():
			shCtx, cancel := context.WithTimeout(context.Background(), s.shutdownTimeout)
			defer cancel()
			// Graceful drain first. If it does not finish within the timeout
			// (e.g. a stuck /media stream), Shutdown returns the deadline
			// error; force-close the remaining connections so we never hang.
			if err := httpSrv.Shutdown(shCtx); err != nil {
				s.logger.Warn("server: graceful shutdown incomplete, forcing close", "error", err)
				_ = httpSrv.Close()
			}
			// Bound the wait for Serve to unwind. Close (and Shutdown on
			// success) make Serve return promptly, but never block Run's exit
			// on it: a wedged serve goroutine must not keep Run alive forever.
			select {
			case <-serveErr:
			case <-time.After(shutdownDrainGrace):
				s.logger.Warn("server: serve goroutine did not unwind after shutdown")
			}
			e := ErrServerClosed
			ctrl.err.Store(&e)
		case e := <-serveErr:
			ctrl.err.Store(&e)
		}
	}()
	return ctrl, nil
}
