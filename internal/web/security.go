package web

import (
	"log"
	"net/http"

	"go-git-server/internal/store"
)

// Settings -> security: where the account is logged in right now (with a
// way to end those sessions) and its recent logins, so a user can spot and
// shut out a login that wasn't them.

type securityData struct {
	Error, Notice string
	Sessions      []sessionInfo
	Logins        []loginRow
}

type loginRow struct {
	store.Login
	Browser string
}

func (s *Server) securityPage(w http.ResponseWriter, r *http.Request, status int, data securityData) {
	name := currentUser(r).Name
	current := ""
	if c, err := r.Cookie(s.cookieName()); err == nil {
		current = c.Value
	}
	data.Sessions = s.sessions.list(name, current)
	logins, err := s.store.RecentLogins(name, 20)
	if err != nil {
		log.Printf("security page: user=%q: %v", name, err)
	}
	for _, l := range logins {
		data.Logins = append(data.Logins, loginRow{l, describeAgent(l.Agent)})
	}
	p := s.newPage(r, "Security", data)
	p.Tab = "security"
	s.render(w, status, "security", p)
}

func (s *Server) handleSecurity(w http.ResponseWriter, r *http.Request) {
	var data securityData
	switch r.URL.Query().Get("done") {
	case "one":
		data.Notice = "That session was logged out."
	case "others":
		data.Notice = "All other sessions were logged out."
	}
	s.securityPage(w, r, http.StatusOK, data)
}

func (s *Server) handleSessionLogout(w http.ResponseWriter, r *http.Request) {
	name := currentUser(r).Name
	if !s.sessions.deleteRef(name, r.PostFormValue("ref")) {
		s.securityPage(w, r, http.StatusBadRequest, securityData{Error: "That session has already ended."})
		return
	}
	log.Printf("session logged out user=%q", name)
	// Ending the current session is a plain logout.
	if currentSession(r) != nil && currentSession(r).ref == r.PostFormValue("ref") {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings/security?done=one", http.StatusSeeOther)
}

func (s *Server) handleLogoutOthers(w http.ResponseWriter, r *http.Request) {
	name := currentUser(r).Name
	current := ""
	if c, err := r.Cookie(s.cookieName()); err == nil {
		current = c.Value
	}
	s.sessions.deleteUserExcept(name, current)
	log.Printf("other sessions logged out user=%q", name)
	http.Redirect(w, r, "/settings/security?done=others", http.StatusSeeOther)
}
