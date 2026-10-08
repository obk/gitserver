package web

import (
	"log"
	"net/http"

	"go-git-server/internal/store"
)

// The audit log records changes to accounts, invites and account security,
// for admins (Settings -> audit log). Repository events are deliberately
// left out: an admin must not learn the names of other users' private
// repositories from it. Entries made with the gitserver command line show
// up here too.

type auditData struct {
	Entries []store.AuditEntry
}

// auditShown is how many entries the page shows.
const auditShown = 200

// audit records action on target by the logged-in user.
func (s *Server) audit(r *http.Request, action, target, detail string) {
	s.auditAs(r, currentUser(r).Name, action, target, detail)
}

// auditAs records action on target by actor, for requests without a
// logged-in user (a signup). Failing to record does not fail the request.
func (s *Server) auditAs(r *http.Request, actor, action, target, detail string) {
	e := store.AuditEntry{Actor: actor, Action: action, Target: target, Detail: detail, IP: s.clientIP(r)}
	if err := s.store.Audit(e); err != nil {
		log.Printf("audit log: %+v: %v", e, err)
	}
}

func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	entries, err := s.store.AuditLog(auditShown)
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "Could not load the audit log.")
		return
	}
	p := s.newPage(r, "Audit log", auditData{entries})
	p.Tab = "audit"
	s.render(w, http.StatusOK, "audit", p)
}
