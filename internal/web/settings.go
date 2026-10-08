package web

import (
	"errors"
	"log"
	"net/http"

	"go-git-server/internal/account"
	"go-git-server/internal/sshgit"
	"go-git-server/internal/store"
)

type keysData struct {
	Keys   []store.SSHKey
	Error  string
	Notice string
	Input  string
	Host   string
}

func (s *Server) keysPage(w http.ResponseWriter, r *http.Request, status int, data keysData) {
	u, err := s.store.Get(currentUser(r).Name)
	if err != nil {
		s.notFound(w, r)
		return
	}
	data.Keys = u.SSHKeys
	data.Host = s.sshHost(r)
	p := s.newPage(r, "SSH keys", data)
	p.Tab = "keys"
	s.render(w, status, "keys", p)
}

func (s *Server) handleKeys(w http.ResponseWriter, r *http.Request) {
	s.keysPage(w, r, http.StatusOK, keysData{})
}

func (s *Server) handleKeyAdd(w http.ResponseWriter, r *http.Request) {
	input := r.PostFormValue("key")
	key, err := sshgit.ParseKey(input)
	if err == nil {
		err = s.store.AddSSHKey(currentUser(r).Name, key)
	}
	if err != nil {
		s.keysPage(w, r, http.StatusBadRequest, keysData{Error: capitalize(err.Error()) + ".", Input: input})
		return
	}
	log.Printf("ssh key added user=%q fingerprint=%s", currentUser(r).Name, key.Fingerprint)
	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

func (s *Server) handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DeleteSSHKey(currentUser(r).Name, r.PostFormValue("id")); err != nil {
		s.keysPage(w, r, http.StatusBadRequest, keysData{Error: capitalize(err.Error()) + "."})
		return
	}
	log.Printf("ssh key deleted user=%q", currentUser(r).Name)
	http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
}

func checkNewPassword(pw, again string) error {
	if n := len([]rune(pw)); n < 12 || len(pw) > 1024 {
		return errors.New("Passwords must be at least 12 characters long.")
	}
	if pw != again {
		return errors.New("The passwords do not match.")
	}
	return nil
}

type passwordData struct {
	Error, Notice string
}

func (s *Server) passwordPage(w http.ResponseWriter, r *http.Request, status int, data passwordData) {
	p := s.newPage(r, "Password", data)
	p.Tab = "password"
	s.render(w, status, "password", p)
}

func (s *Server) handlePassword(w http.ResponseWriter, r *http.Request) {
	var data passwordData
	if r.URL.Query().Get("changed") == "1" {
		data.Notice = "Password changed. All other sessions were logged out."
	} else if sess := currentSession(r); sess != nil && sess.notice != "" {
		data.Error = sess.notice
	}
	s.passwordPage(w, r, http.StatusOK, data)
}

func (s *Server) handlePasswordChange(w http.ResponseWriter, r *http.Request) {
	name := currentUser(r).Name
	userKey := "user:" + name
	if s.limiter.blocked(userKey) {
		s.passwordPage(w, r, http.StatusTooManyRequests, passwordData{Error: "Too many failed attempts. Try again later."})
		return
	}
	u, _, err := s.authenticate(name, r.PostFormValue("current"), r.PostFormValue("totp"))
	if errors.Is(err, account.ErrHashBusy) {
		s.passwordPage(w, r, http.StatusServiceUnavailable, passwordData{Error: capitalize(err.Error()) + "."})
		return
	}
	if u == nil {
		s.limiter.fail(userKey)
		s.passwordPage(w, r, http.StatusUnauthorized, passwordData{Error: "Current password or code is wrong."})
		return
	}
	if err := checkNewPassword(r.PostFormValue("password"), r.PostFormValue("password2")); err != nil {
		s.passwordPage(w, r, http.StatusBadRequest, passwordData{Error: err.Error()})
		return
	}
	hash, err := account.HashPassword(r.PostFormValue("password"))
	if errors.Is(err, account.ErrHashBusy) {
		s.passwordPage(w, r, http.StatusServiceUnavailable, passwordData{Error: capitalize(err.Error()) + "."})
		return
	}
	if err == nil {
		err = s.store.Update(name, func(u *store.User) error { u.PasswordHash = hash; return nil })
	}
	if err != nil {
		s.passwordPage(w, r, http.StatusInternalServerError, passwordData{Error: "Could not change password."})
		return
	}
	// The new password ends every session (see withSession); start a fresh
	// one for this browser.
	s.sessions.deleteUserExcept(name, "")
	if u, err := s.store.Get(name); err == nil {
		s.startSession(w, r, u, "")
	}
	log.Printf("password changed user=%q", name)
	http.Redirect(w, r, "/settings/password?changed=1", http.StatusSeeOther)
}
