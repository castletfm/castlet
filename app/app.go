// Package app is Castlet's composition root. It builds the swappable backends
// from a Config, wires the HTTP server and transcription worker, and owns their
// lifecycles. Swapping a backend (Postgres store, S3 blobs, a cloud
// transcriber) means changing only the constructors here.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/castletfm/castlet/auth"
	authoidc "github.com/castletfm/castlet/auth/oidc"
	"github.com/castletfm/castlet/blob"
	"github.com/castletfm/castlet/blob/localfs"
	"github.com/castletfm/castlet/blob/s3"
	"github.com/castletfm/castlet/config"
	"github.com/castletfm/castlet/internal/session"
	"github.com/castletfm/castlet/queue"
	"github.com/castletfm/castlet/queue/dbqueue"
	"github.com/castletfm/castlet/server"
	"github.com/castletfm/castlet/store"
	"github.com/castletfm/castlet/store/sqlite"
	"github.com/castletfm/castlet/transcribe"
	"github.com/castletfm/castlet/transcribe/command"
	"github.com/castletfm/castlet/transcribe/null"
	"github.com/castletfm/castlet/worker"
)

// App holds the wired-up backends. Build it once with New, then call Migrate
// and Serve (or use the Store directly, as the user-create command does).
type App struct {
	cfg         *config.Config
	store       store.Store
	blobs       blob.BlobStore
	queue       queue.JobQueue
	transcriber transcribe.Transcriber
	sessions    *session.Manager
	authn       auth.Authenticator // nil when OIDC is not configured
	logger      *slog.Logger
}

// New builds the default backends from cfg. It creates the data directory and
// opens the database but does not migrate; call Migrate first.
func New(cfg *config.Config) (*App, error) {
	logger := newLogger(cfg.LogLevel)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("app: create data dir: %w", err)
	}
	st, err := sqlite.Open(filepath.Join(cfg.DataDir, "castlet.db"))
	if err != nil {
		return nil, err
	}
	blobs, err := buildBlobStore(cfg)
	if err != nil {
		return nil, err
	}
	tr, err := buildTranscriber(cfg)
	if err != nil {
		return nil, err
	}
	authn, err := buildAuthenticator(cfg, logger)
	if err != nil {
		return nil, err
	}

	secure := strings.HasPrefix(cfg.BaseURL, "https://")
	return &App{
		cfg:         cfg,
		store:       st,
		blobs:       blobs,
		queue:       dbqueue.New(st),
		transcriber: tr,
		sessions:    session.NewManager(cfg.SessionKey, session.WithSecure(secure)),
		authn:       authn,
		logger:      logger,
	}, nil
}

// Store exposes the metadata store for administrative commands.
func (a *App) Store() store.Store { return a.store }

// Close releases backend resources.
func (a *App) Close() error { return a.store.Close() }

// Migrate applies the database schema.
func (a *App) Migrate(ctx context.Context) error { return a.store.Migrate(ctx) }

// Serve runs the worker and HTTP server until ctx is cancelled, then waits for
// both to shut down. It returns the first terminal error, if any.
func (a *App) Serve(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wk := worker.New(a.store, a.blobs, a.queue, a.transcriber, worker.WithLogger(a.logger))
	wkCtrl, err := wk.Run(ctx)
	if err != nil {
		return fmt.Errorf("app: start worker: %w", err)
	}

	opts := []server.Option{
		server.WithAddr(a.cfg.Addr),
		server.WithBaseURL(a.cfg.BaseURL),
		server.WithSiteName(a.cfg.SiteName),
		server.WithAllowSignup(a.cfg.AllowSignup),
		server.WithLogger(a.logger),
	}
	if a.authn != nil {
		opts = append(opts, server.WithAuthenticator(a.authn))
	}
	srv, err := server.New(a.store, a.blobs, a.queue, a.sessions, opts...)
	if err != nil {
		return err
	}
	srvCtrl, err := srv.Run(ctx)
	if err != nil {
		return err // worker stops via the deferred cancel
	}

	if a.cfg.GeneratedKey {
		a.logger.Warn("CASTLET_SESSION_KEY not set; generated an ephemeral key — sessions will not survive a restart")
	}
	a.logger.Info("castlet started",
		"addr", srvCtrl.Addr(), "base_url", a.cfg.BaseURL, "transcriber", a.cfg.Transcriber)

	<-srvCtrl.Done()
	cancel() // ensure the worker also winds down
	<-wkCtrl.Done()
	return errors.Join(srvCtrl.Err(), wkCtrl.Err())
}

// buildAuthenticator constructs the OIDC authenticator when an issuer is
// configured, performing discovery with a bounded timeout. It returns nil when
// OIDC is disabled.
func buildAuthenticator(cfg *config.Config, logger *slog.Logger) (auth.Authenticator, error) {
	if cfg.OIDCIssuer == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	authn, err := authoidc.New(ctx, authoidc.Config{
		Issuer:       cfg.OIDCIssuer,
		ClientID:     cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret,
		RedirectURL:  cfg.OIDCRedirectURL,
		Scopes:       cfg.OIDCScopes,
	})
	if err != nil {
		return nil, err
	}
	logger.Info("oidc enabled", "issuer", cfg.OIDCIssuer, "redirect_url", cfg.OIDCRedirectURL)
	return authn, nil
}

// buildBlobStore selects the media blob backend. With no config file it is the
// local filesystem under DataDir/media. Otherwise the JSON file's "type" field
// picks the backend ("fs" or "s3") and supplies its settings.
func buildBlobStore(cfg *config.Config) (blob.BlobStore, error) {
	if cfg.BlobStoreConfig == "" {
		return localfs.New(filepath.Join(cfg.DataDir, "media"))
	}
	data, err := os.ReadFile(cfg.BlobStoreConfig)
	if err != nil {
		return nil, fmt.Errorf("app: read blob store config: %w", err)
	}
	var head struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return nil, fmt.Errorf("app: parse blob store config: %w", err)
	}
	switch head.Type {
	case "", "fs":
		var c struct {
			Dir string `json:"dir"`
		}
		_ = json.Unmarshal(data, &c)
		if c.Dir == "" {
			c.Dir = filepath.Join(cfg.DataDir, "media")
		}
		return localfs.New(c.Dir)
	case "s3":
		var c s3.Config
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, fmt.Errorf("app: parse s3 blob store config: %w", err)
		}
		return s3.New(c)
	default:
		return nil, fmt.Errorf("app: unknown blob store type %q", head.Type)
	}
}

func buildTranscriber(cfg *config.Config) (transcribe.Transcriber, error) {
	switch cfg.Transcriber {
	case "", "null":
		return null.New(), nil
	case "command":
		if cfg.TranscribeCommand == "" {
			return nil, errors.New("app: transcriber=command requires --transcribe-command")
		}
		return command.New(cfg.TranscribeCommand, command.WithArgs(cfg.TranscribeArgs...)), nil
	default:
		return nil, fmt.Errorf("app: unknown transcriber %q", cfg.Transcriber)
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
