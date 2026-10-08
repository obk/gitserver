package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"

	"go-git-server/internal/account"
	"go-git-server/internal/gitrepo"
)

// Deleting your own account (Settings -> security): the account, its SSH
// keys and its unused invites go at once, and so do all its repositories.
// The name stays taken for good, so nobody can step into the old account's
// place. It needs the password, a 2FA code and the user name typed out.

func (s *Server) handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	name := currentUser(r).Name
	userKey := "user:" + name
	fail := func(status int, msg string) {
		s.securityPage(w, r, status, securityData{DeleteError: msg})
	}
	if s.limiter.blocked(userKey) {
		fail(http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	if r.PostFormValue("confirm") != name {
		fail(http.StatusBadRequest, "Type your user name to confirm.")
		return
	}
	u, _, _, err := s.authenticate(name, r.PostFormValue("current"), r.PostFormValue("totp"))
	if errors.Is(err, account.ErrHashBusy) {
		fail(http.StatusServiceUnavailable, capitalize(err.Error())+".")
		return
	}
	if u == nil {
		s.limiter.fail(userKey)
		fail(http.StatusUnauthorized, "Password or code is wrong.")
		return
	}
	if u.Admin {
		users, err := s.store.List()
		if err != nil {
			fail(http.StatusInternalServerError, "Could not delete the account.")
			return
		}
		admins := 0
		for _, other := range users {
			if other.Admin {
				admins++
			}
		}
		if admins == 1 {
			fail(http.StatusConflict, "You are the only admin. Make someone else an admin first (gitserverctl user admin NAME true on the server).")
			return
		}
	}
	repos, _ := gitrepo.List(s.reposDir, name)
	if err := s.store.Delete(name); err != nil {
		log.Printf("account delete: user=%q: %v", name, err)
		fail(http.StatusInternalServerError, "Could not delete the account.")
		return
	}
	// The account is gone, so nothing can reach it any more: no sessions,
	// no SSH key. Now its repositories.
	s.sessions.deleteUserExcept(name, "")
	s.totpSetups.delete(name)
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		Secure: !s.cfg.Insecure, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	if err := gitrepo.DeleteOwner(s.reposDir, name); err != nil {
		log.Printf("account delete: user=%q: deleting repositories: %v", name, err)
	}
	log.Printf("account deleted user=%q repos=%d ip=%s", name, len(repos), s.clientIP(r))
	s.auditAs(r, name, "account deleted", name, fmt.Sprintf("%d repositories deleted", len(repos)))
	p := s.newPage(r, "Account deleted", nil)
	p.User = nil
	s.render(w, http.StatusOK, "deleted", p)
}
