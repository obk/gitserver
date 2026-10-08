package web

import (
	"log"
	"net/http"

	"go-git-server/internal/store"
)

// The audit log records changes to accounts, invites and account
// security. Each user sees their own (Settings -> audit log): what they
// changed, and what was changed on their account, including by the server
// operator with gitserverctl. Repository events are left out. The whole log
// is only on the server: gitserver audit.

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
	entries, err := s.store.AuditLog(currentUser(r).Name, auditShown)
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "Could not load the audit log.")
		return
	}
	p := s.newPage(r, "Audit log", auditData{entries})
	p.Tab = "audit"
	s.render(w, http.StatusOK, "audit", p)
}
