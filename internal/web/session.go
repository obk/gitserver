package web

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"sync"
	"time"

	"go-git-server/internal/account"
	"go-git-server/internal/store"
)

// Sessions live in memory; restarting the server logs everyone out.
const (
	sessionMaxAge = 12 * time.Hour
	sessionIdle   = 2 * time.Hour
)

type session struct {
	user   string
	credFP string // credentialFingerprint at login
	csrf   string
	notice string // security warning shown on the password and 2FA pages
	// newCodes are recovery codes just generated for this session's user,
	// shown once on the 2FA page (takeNewCodes) and then forgotten.
	newCodes []string
	created  time.Time
	seen     time.Time
}

type sessionStore struct {
	mu sync.Mutex
	m  map[string]*session // keyed by SHA-256 of the cookie value
}

func newSessionStore() *sessionStore {
	s := &sessionStore{m: make(map[string]*session)}
	go func() {
		for range time.Tick(10 * time.Minute) {
			s.gc()
		}
	}()
	return s
}

// credentialFingerprint changes whenever the password or TOTP secret
// changes, which ends all existing sessions of that user.
func credentialFingerprint(u *store.User) string {
	return account.HashToken(u.PasswordHash + "\x00" + u.TOTPSecret)
}

// setNewCodes keeps freshly generated recovery codes for the session with
// id until the 2FA page shows them.
func (s *sessionStore) setNewCodes(id string, codes []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.m[account.HashToken(id)]; sess != nil {
		sess.newCodes = codes
	}
}

// takeNewCodes returns and forgets the codes set by setNewCodes.
func (s *sessionStore) takeNewCodes(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.m[account.HashToken(id)]
	if sess == nil {
		return nil
	}
	codes := sess.newCodes
	sess.newCodes = nil
	return codes
}

func (s *sessionStore) create(user, credFP, notice string) string {
	id := account.RandomToken(32)
	now := time.Now()
	s.mu.Lock()
	s.m[account.HashToken(id)] = &session{user: user, credFP: credFP, csrf: account.RandomToken(32), notice: notice, created: now, seen: now}
	s.mu.Unlock()
	return id
}

func (s *sessionStore) get(id string) (session, bool) {
	key := account.HashToken(id)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[key]
	if !ok {
		return session{}, false
	}
	if now.Sub(sess.created) > sessionMaxAge || now.Sub(sess.seen) > sessionIdle {
		delete(s.m, key)
		return session{}, false
	}
	sess.seen = now
	return *sess, true
}

func (s *sessionStore) delete(id string) {
	s.mu.Lock()
	delete(s.m, account.HashToken(id))
	s.mu.Unlock()
}

// deleteUserExcept ends all sessions of user other than the one with id keep.
func (s *sessionStore) deleteUserExcept(user, keep string) {
	keepKey := account.HashToken(keep)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, sess := range s.m {
		if sess.user == user && k != keepKey {
			delete(s.m, k)
		}
	}
}

func (s *sessionStore) gc() {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, sess := range s.m {
		if now.Sub(sess.created) > sessionMaxAge || now.Sub(sess.seen) > sessionIdle {
			delete(s.m, k)
		}
	}
}

func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
)

// withSession attaches the logged-in user (if any) to the request context.
// A session ends when the user is deleted or their password or TOTP secret
// changes (also when changed from the command line).
func (s *Server) withSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(s.cookieName()); err == nil {
			if sess, ok := s.sessions.get(c.Value); ok {
				u, err := s.store.Get(sess.user)
				if err == nil && constantTimeEqual(sess.credFP, credentialFingerprint(u)) {
					ctx := context.WithValue(r.Context(), ctxUser, u)
					ctx = context.WithValue(ctx, ctxSession, &sess)
					r = r.WithContext(ctx)
				} else {
					s.sessions.delete(c.Value)
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// requireSSHKey sends logged-in users without an SSH key to the key page:
// every account must have at least one.
func (s *Server) requireSSHKey(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if u := currentUser(r); u != nil && len(u.SSHKeys) == 0 {
			switch r.URL.Path {
			case "/settings/keys", "/logout", "/static/style.css", "/static/highlight.css":
			default:
				http.Redirect(w, r, "/settings/keys", http.StatusSeeOther)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func currentUser(r *http.Request) *store.User {
	u, _ := r.Context().Value(ctxUser).(*store.User)
	return u
}

func currentSession(r *http.Request) *session {
	sess, _ := r.Context().Value(ctxSession).(*session)
	return sess
}

func (s *Server) requireUser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if currentUser(r) == nil {
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			} else {
				s.notFound(w, r)
			}
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		if r.Method == http.MethodPost && !s.validCSRF(r) {
			s.error(w, r, http.StatusForbidden, "Invalid or missing CSRF token.")
			return
		}
		h(w, r)
	}
}

func (s *Server) requireAdmin(h http.HandlerFunc) http.HandlerFunc {
	return s.requireUser(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).Admin {
			s.notFound(w, r)
			return
		}
		h(w, r)
	})
}

// startSession issues a fresh session ID (preventing session fixation).
// A non-empty notice is shown on the password and 2FA pages during this
// session. It returns the new session's ID.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, u *store.User, notice string) string {
	if c, err := r.Cookie(s.cookieName()); err == nil {
		s.sessions.delete(c.Value)
	}
	id := s.sessions.create(u.Name, credentialFingerprint(u), notice)
	http.SetCookie(w, &http.Cookie{
		Name:     s.cookieName(),
		Value:    id,
		Path:     "/",
		MaxAge:   int(sessionMaxAge.Seconds()),
		Secure:   !s.cfg.Insecure,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
	})
	return id
}

func (s *Server) validCSRF(r *http.Request) bool {
	sess := currentSession(r)
	if sess == nil {
		return false
	}
	got := r.PostFormValue("csrf")
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(sess.csrf)) == 1
}
