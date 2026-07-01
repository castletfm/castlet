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
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/castletfm/castlet/app"
	"github.com/castletfm/castlet/config"
	emailpkg "github.com/castletfm/castlet/internal/email"
	"github.com/castletfm/castlet/internal/idgen"
	"github.com/castletfm/castlet/model"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/term"
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
	passwordFile := fs.String("password-file", "", "read the password from this file (trailing newline trimmed); preferred over --password")
	password := fs.String("password", "", "password (INSECURE: visible in shell history and the process list; prefer --password-file or the interactive prompt)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *email == "" {
		return fmt.Errorf("--email is required")
	}
	// Canonicalize so the CLI creates accounts keyed on the same mailbox form as
	// signup and OIDC: a malformed address is rejected, and the stored value is
	// the bare, lower-cased address.
	canonicalEmail, err := emailpkg.Canonical(*email)
	if err != nil {
		return fmt.Errorf("invalid --email: %w", err)
	}

	// Determine which password source flags were actually supplied on the
	// command line, so precedence keys on whether a flag was provided rather
	// than on whether its value happens to be non-empty. This makes an explicit
	// --password "" or --password-file "" behave as the user selected them.
	var passwordFileSet, passwordSet bool
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "password-file":
			passwordFileSet = true
		case "password":
			passwordSet = true
		}
	})

	plaintext, err := resolvePassword(*passwordFile, passwordFileSet, *password, passwordSet, os.Stdin, os.Stderr)
	if err != nil {
		return err
	}
	if plaintext == "" {
		return fmt.Errorf("a password is required (use --password-file, the interactive prompt, or --password)")
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

	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	display := *name
	if display == "" {
		display = canonicalEmail
	}
	user := &model.User{
		ID:           idgen.New(),
		Email:        canonicalEmail,
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

// resolvePassword returns the plaintext password using the following
// precedence, keyed on which flags were SUPPLIED (fileSet/flagSet) rather than
// on whether their values are non-empty: (1) --password-file if supplied (this
// source wins even when --password is also given; an empty or unreadable path
// is a clear error); (2) the --password flag if supplied (returned verbatim,
// even when empty, so the caller's empty-password validation can reject it);
// (3) an interactive prompt when stdin is a terminal; (4) otherwise an error,
// since no password source is available. in is the file used to detect and read
// from a terminal, out is where the prompt is written.
func resolvePassword(passwordFile string, fileSet bool, passwordFlag string, flagSet bool, in *os.File, out io.Writer) (string, error) {
	if fileSet {
		if passwordFile == "" {
			return "", fmt.Errorf("--password-file requires a path")
		}
		return readPasswordFile(passwordFile)
	}
	if flagSet {
		return passwordFlag, nil
	}
	if in != nil && term.IsTerminal(int(in.Fd())) {
		return promptPassword(int(in.Fd()), out)
	}
	return "", fmt.Errorf("no password source: use --password-file, --password, or run interactively for a prompt")
}

// readPasswordFile reads a password from path, trimming any trailing newline
// (and carriage return) left by editors.
func readPasswordFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read password file: %w", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

// promptPassword reads a password from the terminal identified by fd without
// echoing it, writing the prompt to out.
func promptPassword(fd int, out io.Writer) (string, error) {
	fmt.Fprint(out, "Password: ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	if err != nil {
		return "", fmt.Errorf("read password: %w", err)
	}
	return string(b), nil
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
  castlet user-create   create an admin user (--email, --password-file, [--name])
  castlet version       print the version

Run "castlet serve -h" for serve flags. Configuration also reads CASTLET_*
environment variables.
`)
}
