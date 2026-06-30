package server

import (
	"net/http"

	"golang.org/x/crypto/bcrypt"
)

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

	s.sessions.Issue(w, user.ID, s.now())
	s.redirect(w, r, "/admin/")
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.sessions.Clear(w)
	s.redirect(w, r, "/")
}
