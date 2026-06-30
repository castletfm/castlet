// Command castlet is the Castlet podcast server CLI.
//
// Usage:
//
//	castlet serve         run the web server and transcription worker
//	castlet migrate       create or upgrade the database schema
//	castlet user-create   create an admin user
//	castlet version       print the version
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/castletfm/castlet/app"
	"github.com/castletfm/castlet/config"
	"github.com/castletfm/castlet/internal/idgen"
	"github.com/castletfm/castlet/model"
	"golang.org/x/crypto/bcrypt"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "migrate":
		err = cmdMigrate(args)
	case "user-create":
		err = cmdUserCreate(args)
	case "version", "-v", "--version":
		fmt.Println("castlet", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "castlet: unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "castlet:", err)
		os.Exit(1)
	}
}

func cmdServe(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	application, err := app.New(cfg)
	if err != nil {
		return err
	}
	defer application.Close()

	ctx := context.Background()
	if err := application.Migrate(ctx); err != nil {
		return err
	}

	// Stop cleanly on Ctrl-C / SIGTERM.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return application.Serve(ctx)
}

func cmdMigrate(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}
	application, err := app.New(cfg)
	if err != nil {
		return err
	}
	defer application.Close()

	if err := application.Migrate(context.Background()); err != nil {
		return err
	}
	fmt.Println("migration complete")
	return nil
}

func cmdUserCreate(args []string) error {
	fs := flag.NewFlagSet("user-create", flag.ContinueOnError)
	dataDir := fs.String("data-dir", envOr("CASTLET_DATA_DIR", "./data"), "data directory")
	email := fs.String("email", "", "user email (required)")
	name := fs.String("name", "", "display name (defaults to the email)")
	password := fs.String("password", "", "password (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" || *password == "" {
		return fmt.Errorf("--email and --password are required")
	}

	// Build a minimal config just for store access.
	cfg := &config.Config{DataDir: *dataDir, LogLevel: "warn"}
	application, err := app.New(cfg)
	if err != nil {
		return err
	}
	defer application.Close()

	ctx := context.Background()
	if err := application.Migrate(ctx); err != nil {
		return err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(*password), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	display := *name
	if display == "" {
		display = *email
	}
	user := &model.User{
		ID:           idgen.New(),
		Email:        *email,
		DisplayName:  display,
		PasswordHash: string(hash),
		CreatedAt:    time.Now(),
	}
	if err := application.Store().CreateUser(ctx, user); err != nil {
		return err
	}
	fmt.Printf("created user %s (%s)\n", user.Email, user.ID)
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprint(os.Stderr, `castlet — a minimalist, self-hostable podcast server

Usage:
  castlet serve         run the web server and transcription worker
  castlet migrate       create or upgrade the database schema
  castlet user-create   create an admin user (--email, --password, [--name])
  castlet version       print the version

Run "castlet serve -h" for serve flags. Configuration also reads CASTLET_*
environment variables.
`)
}
