package web

import (
	"errors"
	"html/template"
	"log"
	"net/http"
	"sync"
	"time"

	"go-git-server/internal/account"
	"go-git-server/internal/store"
)

// Settings -> two-factor: recovery codes (shown once, right after they are
// made) and setting up a new authenticator, so a user who lost their phone
// and logged in with a recovery code needs no admin. Both actions ask for
// the current password, so a session left open is not enough.

type twoFactorData struct {
	Error, Notice string
	CodesLeft     int
	NewCodes      []string // shown this once
	// Setting up a new authenticator: scan, then confirm with one code.
	Setup  bool
	QR     template.HTML
	Secret string
}

// totpSetups holds the secret of an authenticator being set up, per user,
// until it is confirmed with a code from it.
type totpSetups struct {
	mu sync.Mutex
	m  map[string]totpSetup
}

type totpSetup struct {
	secret  string
	expires time.Time
}

const totpSetupTTL = 15 * time.Minute

func (t *totpSetups) put(user, secret string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	for u, s := range t.m { // drop abandoned setups
		if now.After(s.expires) {
			delete(t.m, u)
		}
	}
	t.m[user] = totpSetup{secret: secret, expires: now.Add(totpSetupTTL)}
}

func (t *totpSetups) get(user string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[user]
	if !ok || time.Now().After(s.expires) {
		return "", false
	}
	return s.secret, true
}

func (t *totpSetups) delete(user string) {
	t.mu.Lock()
	delete(t.m, user)
	t.mu.Unlock()
}

func (s *Server) twoFactorPage(w http.ResponseWriter, r *http.Request, status int, data twoFactorData) {
	name := currentUser(r).Name
	left, err := s.store.RecoveryCodesLeft(name)
	if err != nil {
		log.Printf("2fa page: user=%q: %v", name, err)
	}
	data.CodesLeft = left
	p := s.newPage(r, "Two-factor authentication", data)
	p.Tab = "2fa"
	s.render(w, status, "twofactor", p)
}

func (s *Server) handleTwoFactor(w http.ResponseWriter, r *http.Request) {
	var data twoFactorData
	if c, err := r.Cookie(s.cookieName()); err == nil {
		data.NewCodes = s.sessions.takeNewCodes(c.Value)
	}
	switch {
	case r.URL.Query().Get("totp") == "1":
		data.Notice = "New authenticator set up; codes from the old one no longer work. All other sessions were logged out."
	case len(data.NewCodes) > 0 && r.URL.Query().Get("welcome") == "1":
		data.Notice = "Welcome! Your account is ready. Save your recovery codes now: they are shown only this once."
	default:
		if sess := currentSession(r); sess != nil && sess.notice != "" {
			data.Error = sess.notice
		}
	}
	s.twoFactorPage(w, r, http.StatusOK, data)
}

// checkCurrentPassword re-checks the logged-in user's password before a
// change to their 2FA. Wrong passwords count against the account's limit.
// It renders an error page and returns false if the password is not right.
func (s *Server) checkCurrentPassword(w http.ResponseWriter, r *http.Request) bool {
	name := currentUser(r).Name
	userKey := "user:" + name
	if s.limiter.blocked(userKey) {
		s.twoFactorPage(w, r, http.StatusTooManyRequests, twoFactorData{Error: "Too many failed attempts. Try again later."})
		return false
	}
	u, err := s.store.Get(name)
	if err != nil {
		s.notFound(w, r)
		return false
	}
	ok, err := account.CheckPassword(u.PasswordHash, r.PostFormValue("current"))
	if errors.Is(err, account.ErrHashBusy) {
		s.twoFactorPage(w, r, http.StatusServiceUnavailable, twoFactorData{Error: capitalize(err.Error()) + "."})
		return false
	}
	if !ok {
		s.limiter.fail(userKey)
		s.twoFactorPage(w, r, http.StatusUnauthorized, twoFactorData{Error: "Current password is wrong."})
		return false
	}
	return true
}

// newRecoveryCodes replaces user's recovery codes and keeps the new ones in
// the session (sessionID) for the 2FA page to show once.
func (s *Server) newRecoveryCodes(user, sessionID string) error {
	codes := account.NewRecoveryCodes()
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = account.HashRecoveryCode(user, c)
	}
	if err := s.store.SetRecoveryCodes(user, hashes); err != nil {
		return err
	}
	s.sessions.setNewCodes(sessionID, codes)
	return nil
}

func (s *Server) handleRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentPassword(w, r) {
		return
	}
	name := currentUser(r).Name
	c, err := r.Cookie(s.cookieName())
	if err == nil {
		err = s.newRecoveryCodes(name, c.Value)
	}
	if err != nil {
		log.Printf("recovery codes: user=%q: %v", name, err)
		s.twoFactorPage(w, r, http.StatusInternalServerError, twoFactorData{Error: "Could not create recovery codes."})
		return
	}
	log.Printf("recovery codes created user=%q", name)
	// Shown by the GET, so reloading the page can't make another set.
	http.Redirect(w, r, "/settings/2fa", http.StatusSeeOther)
}

func (s *Server) handleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	if !s.checkCurrentPassword(w, r) {
		return
	}
	name := currentUser(r).Name
	secret := account.NewTOTPSecret()
	s.totpSetups.put(name, secret)
	s.renderTOTPSetup(w, r, http.StatusOK, secret, "")
}

func (s *Server) renderTOTPSetup(w http.ResponseWriter, r *http.Request, status int, secret, errMsg string) {
	svg, err := qrSVG(account.TOTPURI(s.cfg.SiteName, currentUser(r).Name, secret))
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "Could not create QR code.")
		return
	}
	s.twoFactorPage(w, r, status, twoFactorData{Setup: true, QR: svg, Secret: secret, Error: errMsg})
}

func (s *Server) handleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	name := currentUser(r).Name
	userKey := "user:" + name
	if s.limiter.blocked(userKey) {
		s.twoFactorPage(w, r, http.StatusTooManyRequests, twoFactorData{Error: "Too many failed attempts. Try again later."})
		return
	}
	secret, ok := s.totpSetups.get(name)
	if !ok {
		s.twoFactorPage(w, r, http.StatusBadRequest, twoFactorData{Error: "The setup expired. Start again."})
		return
	}
	step, ok := account.CheckTOTP(secret, r.PostFormValue("totp"), 0, time.Now())
	if !ok {
		s.limiter.fail(userKey)
		s.renderTOTPSetup(w, r, http.StatusBadRequest, secret, "That code is not valid. Check that your device's clock is correct.")
		return
	}
	sealed := s.box.SealTOTP(name, secret)
	if err := s.store.Update(name, func(u *store.User) error { u.TOTPSecret, u.TOTPLast = sealed, step; return nil }); err != nil {
		log.Printf("2fa setup: user=%q: %v", name, err)
		s.twoFactorPage(w, r, http.StatusInternalServerError, twoFactorData{Error: "Could not save the new authenticator."})
		return
	}
	s.totpSetups.delete(name)
	// A new TOTP secret ends every session (see withSession); start a fresh
	// one for this browser.
	s.sessions.deleteUserExcept(name, "")
	if u, err := s.store.Get(name); err == nil {
		s.startSession(w, r, u, "")
	}
	log.Printf("new authenticator user=%q", name)
	http.Redirect(w, r, "/settings/2fa?totp=1", http.StatusSeeOther)
}
