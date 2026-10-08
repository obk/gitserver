package web

import (
	"log"
	"net/http"
	"time"

	"go-git-server/internal/store"
)

type invitesData struct {
	Invites []*store.Invite
	NewLink string
	Error   string
	Now     time.Time
}

func (s *Server) invitesPage(w http.ResponseWriter, r *http.Request, status int, data invitesData) {
	list, err := s.store.Invites()
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "Could not load invites.")
		return
	}
	data.Invites, data.Now = list, time.Now()
	p := s.newPage(r, "Invites", data)
	p.Tab = "invites"
	s.render(w, status, "invites", p)
}

func (s *Server) handleInvites(w http.ResponseWriter, r *http.Request) {
	s.invitesPage(w, r, http.StatusOK, invitesData{})
}

var inviteTTLs = map[string]time.Duration{"1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

func (s *Server) handleInviteCreate(w http.ResponseWriter, r *http.Request) {
	ttl, ok := inviteTTLs[r.PostFormValue("expires")]
	if !ok {
		s.invitesPage(w, r, http.StatusBadRequest, invitesData{Error: "Invalid expiry."})
		return
	}
	admin := r.PostFormValue("admin") == "on"
	code, inv, err := s.store.CreateInvite(currentUser(r).Name, admin, ttl)
	if err != nil {
		s.invitesPage(w, r, http.StatusInternalServerError, invitesData{Error: "Could not create invite."})
		return
	}
	log.Printf("invite created by=%q admin=%v", currentUser(r).Name, admin)
	detail := "id " + inv.ID + ", expires " + inv.Expires.UTC().Format("2006-01-02 15:04 UTC")
	if admin {
		detail += ", makes an admin"
	}
	s.audit(r, "invite created", "", detail)
	s.invitesPage(w, r, http.StatusOK, invitesData{NewLink: s.baseURL(r) + "/signup?code=" + code})
}

func (s *Server) handleInviteRevoke(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	if err := s.store.RevokeInvite(id); err != nil {
		s.invitesPage(w, r, http.StatusBadRequest, invitesData{Error: "Could not revoke invite."})
		return
	}
	s.audit(r, "invite revoked", "", "id "+id)
	http.Redirect(w, r, "/settings/invites", http.StatusSeeOther)
}
