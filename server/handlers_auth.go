package server

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/castletfm/castlet/internal/idgen"
	"github.com/castletfm/castlet/model"
	"github.com/castletfm/castlet/store"
	"golang.org/x/crypto/bcrypt"
)

// minPasswordLen is the minimum length for a local account password.
const minPasswordLen = 8

type loginPage struct {
	Email string
	Error string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if userFrom(r.Context()) != nil {
		s.redirect(w, r, "/admin/")
		return
	}
	s.render(w, r, http.StatusOK, "login", "Log in", loginPage{})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	password := r.FormValue("password")

	user, err := s.store.UserByEmail(r.Context(), email)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
		// Same response whether the email is unknown or the password is wrong,
		// so the form does not reveal which accounts exist.
		s.render(w, r, http.StatusUnauthorized, "login", "Log in",
			loginPage{Email: email, Error: "Invalid email or password."})
		return
	}

	s.sessions.Issue(w, user.ID, user.SessionEpoch, s.now())
	s.redirect(w, r, "/admin/")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	// Bump the epoch so every session for this user (not just this browser's
	// cookie) is invalidated — "log out everywhere". Clearing the cookie only
	// affects the current client.
	if u := userFrom(r.Context()); u != nil {
		if err := s.store.BumpSessionEpoch(r.Context(), u.ID); err != nil {
			s.logger.Warn("failed to bump session epoch on logout", "user", u.ID, "error", err)
		}
	}
	s.sessions.Clear(w)
	s.redirect(w, r, "/")
}

type signupPage struct {
	Email string
	Name  string
	Error string
}

func (s *Server) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	if !s.allowSignup {
		s.renderError(w, r, http.StatusNotFound, "Sign-up is disabled.")
		return
	}
	if userFrom(r.Context()) != nil {
		s.redirect(w, r, "/admin/")
		return
	}
	s.render(w, r, http.StatusOK, "signup", "Sign up", signupPage{})
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	if !s.allowSignup {
		s.renderError(w, r, http.StatusNotFound, "Sign-up is disabled.")
		return
	}

	email := strings.TrimSpace(r.FormValue("email"))
	name := strings.TrimSpace(r.FormValue("name"))
	password := r.FormValue("password")
	confirm := r.FormValue("password_confirm")

	form := signupPage{Email: email, Name: name}
	fail := func(msg string) {
		form.Error = msg
		s.render(w, r, http.StatusBadRequest, "signup", "Sign up", form)
	}

	if _, err := mail.ParseAddress(email); err != nil {
		fail("Enter a valid email address.")
		return
	}
	if len(password) < minPasswordLen {
		fail("Password must be at least 8 characters.")
		return
	}
	if password != confirm {
		fail("Passwords do not match.")
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if name == "" {
		name = email
	}
	user := &model.User{
		ID:           idgen.New(),
		Email:        email,
		DisplayName:  name,
		PasswordHash: string(hash),
		CreatedAt:    s.now(),
	}
	if err := s.store.CreateUser(r.Context(), user); err != nil {
		if errors.Is(err, store.ErrConflict) {
			fail("That email is already registered.")
			return
		}
		s.serverError(w, r, err)
		return
	}

	s.sessions.Issue(w, user.ID, user.SessionEpoch, s.now())
	s.redirect(w, r, "/admin/")
}
