package web

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go-git-server/internal/account"
	"go-git-server/internal/store"
)

// safeNext only allows redirects to local paths.
func safeNext(next string) string {
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") {
		return "/"
	}
	// Browsers drop tabs and newlines and treat \ like /, so "/\t/evil.com"
	// would become "//evil.com". Reject all control characters and backslashes.
	if strings.ContainsFunc(next, func(c rune) bool { return c < 0x20 || c == 0x7f || c == '\\' }) {
		return "/"
	}
	if u, err := url.Parse(next); err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return next
}

type loginData struct {
	Next  string
	Error string
	Name  string
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	next := safeNext(r.URL.Query().Get("next"))
	if currentUser(r) != nil {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	s.render(w, http.StatusOK, "login", s.newPage(r, "Log in", loginData{Next: next}))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	name := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	code := r.PostFormValue("code")
	next := safeNext(r.PostFormValue("next"))
	ip := s.clientIP(r)
	ipKey, userKey := ipKey(ip), "user:"+name

	fail := func(status int, msg string) {
		s.render(w, status, "login", s.newPage(r, "Log in", loginData{Next: next, Error: msg, Name: name}))
	}
	// The attempt counts against the IP before the password is hashed, so
	// parallel requests cannot all slip past the limit; success refunds it.
	if s.limiter.blocked(userKey) || !s.limiter.take(ipKey) {
		fail(http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	u, passwordOK, err := s.authenticate(name, password, code)
	if errors.Is(err, account.ErrHashBusy) {
		s.limiter.refund(ipKey)
		fail(http.StatusServiceUnavailable, capitalize(err.Error())+".")
		return
	}
	if u == nil {
		// Only wrong codes after a correct password count against the
		// account. Wrong passwords count per IP only, so a stranger who
		// types random passwords cannot lock the real user out.
		if passwordOK {
			s.limiter.fail(userKey)
			if !errors.Is(err, errCodeReused) {
				s.alerts.add(name)
			}
		}
		log.Printf("login failed user=%q ip=%s password_ok=%v", name, ip, passwordOK)
		fail(http.StatusUnauthorized, "Invalid username, password or code.")
		return
	}
	s.limiter.refund(ipKey)
	s.limiter.reset(userKey)
	var notice string
	if a := s.alerts.take(u.Name); a.n > 0 {
		notice = fmt.Sprintf("Since %s, %d wrong 2FA code(s) were entered together with your correct password. "+
			"If that was not you, someone knows your password: change it now.", a.since.UTC().Format("2006-01-02 15:04 MST"), a.n)
		next = "/settings/password"
		log.Printf("login ok user=%q ip=%s after %d wrong code(s) with the correct password", name, ip, a.n)
	} else {
		log.Printf("login ok user=%q ip=%s", name, ip)
	}
	s.startSession(w, r, u, notice)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// authenticate checks password and TOTP code and returns the user on
// success. passwordOK reports whether the password alone was right. A TOTP
// code is only consumed once the password has been verified. The error is
// account.ErrHashBusy when the server is too busy to check the password, or
// errCodeReused when the code was right but already used.
func (s *Server) authenticate(name, password, code string) (u *store.User, passwordOK bool, err error) {
	if len(password) > 1024 {
		return nil, false, nil
	}
	u, err = s.store.Get(name)
	if err != nil {
		_, err := account.CheckPassword(account.DummyHash(), password)
		return nil, false, err
	}
	if ok, err := account.CheckPassword(u.PasswordHash, password); !ok {
		return nil, false, err
	}
	err = s.store.Update(name, func(stored *store.User) error {
		secret, err := s.box.OpenTOTP(stored.Name, stored.TOTPSecret)
		if err != nil {
			log.Printf("login: user=%q: %v", stored.Name, err)
			return err
		}
		now := time.Now()
		step, ok := account.CheckTOTP(secret, code, stored.TOTPLast, now)
		if !ok {
			if _, valid := account.CheckTOTP(secret, code, 0, now); valid {
				return errCodeReused
			}
			return errors.New("bad code")
		}
		stored.TOTPLast = step
		u = stored
		return nil
	})
	if errors.Is(err, errCodeReused) {
		return nil, true, err
	}
	if err != nil {
		return nil, true, nil
	}
	return u, true, nil
}

// errCodeReused: the code is valid but was already used, e.g. a form sent
// twice. It fails like any wrong code but is no sign of an attack.
var errCodeReused = errors.New("2FA code already used")

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if currentUser(r) != nil && !s.validCSRF(r) {
		s.error(w, r, http.StatusForbidden, "Invalid or missing CSRF token.")
		return
	}
	if c, err := r.Cookie(s.cookieName()); err == nil {
		s.sessions.delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		Secure: !s.cfg.Insecure, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
