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
		// #nosec G710 -- next was checked by safeNext
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
	u, passwordOK, viaRecovery, err := s.authenticate(name, password, code)
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
				if err := s.store.AddCodeFailure(name); err != nil {
					log.Printf("login: recording wrong code of user=%q: %v", name, err)
				}
			}
		}
		log.Printf("login failed user=%q ip=%s password_ok=%v", name, ip, passwordOK)
		fail(http.StatusUnauthorized, "Invalid username, password or code.")
		return
	}
	s.limiter.refund(ipKey)
	s.limiter.reset(userKey)
	// Wrong codes entered with the correct password mean someone may know
	// it: warn the user instead of locking the account (which would let that
	// person lock the real user out).
	var notices []string
	if viaRecovery {
		left, _ := s.store.RecoveryCodesLeft(u.Name)
		notices = append(notices, fmt.Sprintf("You logged in with a recovery code; it won't work again (%d left). "+
			"If you lost your authenticator, set up a new one below.", left))
		next = "/settings/2fa"
		log.Printf("login with a recovery code user=%q ip=%s left=%d", name, ip, left)
	}
	failures, since, err := s.store.TakeCodeFailures(u.Name)
	if err != nil {
		log.Printf("login: reading wrong codes of user=%q: %v", u.Name, err)
	}
	if failures > 0 {
		notices = append(notices, fmt.Sprintf("Since %s, %d wrong 2FA code(s) were entered together with your correct password. "+
			"If that was not you, someone knows your password: change it now.", since.UTC().Format("2006-01-02 15:04 MST"), failures))
		next = "/settings/password"
		log.Printf("login ok user=%q ip=%s after %d wrong code(s) with the correct password", name, ip, failures)
	} else {
		log.Printf("login ok user=%q ip=%s", name, ip)
	}
	notice := strings.Join(notices, " ")
	method := "password and authenticator code"
	if viaRecovery {
		method = "password and recovery code"
	}
	id := s.startSession(w, r, u, notice)
	s.recordLogin(id, u.Name, ip, r.UserAgent(), method)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// recordLogin adds a login to the user's history (Settings -> security)
// and tells the new session, once, when and from where the previous
// login was, so a login that wasn't the user stands out.
func (s *Server) recordLogin(sessionID, user, ip, agent, method string) {
	prev, err := s.store.RecentLogins(user, 1)
	if err != nil {
		log.Printf("login history: user=%q: %v", user, err)
	}
	if err := s.store.RecordLogin(user, store.Login{At: time.Now(), IP: ip, Agent: agent, Method: method}); err != nil {
		log.Printf("login history: user=%q: %v", user, err)
	}
	if len(prev) == 1 {
		s.sessions.setFlash(sessionID, fmt.Sprintf("Last login: %s from %s (%s). Not you? See Settings → security.",
			prev[0].At.UTC().Format("2006-01-02 15:04 MST"), prev[0].IP, describeAgent(prev[0].Agent)))
	}
}

// authenticate checks password and TOTP code and returns the user on
// success. passwordOK reports whether the password alone was right. A TOTP
// code is only consumed once the password has been verified. The error is
// account.ErrHashBusy when the server is too busy to check the password, or
// errCodeReused when the code was right but already used.
//
// Instead of a code from the authenticator, code may be one of the user's
// recovery codes (a lost phone). It then works only this once, and
// viaRecovery is set.
func (s *Server) authenticate(name, password, code string) (u *store.User, passwordOK, viaRecovery bool, err error) {
	if len(password) > 1024 {
		return nil, false, false, nil
	}
	u, err = s.store.Get(name)
	if err != nil {
		_, err := account.CheckPassword(account.DummyHash(), password)
		return nil, false, false, err
	}
	if ok, err := account.CheckPassword(u.PasswordHash, password); !ok {
		return nil, false, false, err
	}
	if account.NormalizeRecoveryCode(code) != "" {
		used, err := s.store.UseRecoveryCode(name, account.HashRecoveryCode(name, code))
		if err != nil {
			log.Printf("login: user=%q: recovery code: %v", name, err)
		}
		if !used {
			return nil, true, false, nil
		}
		return u, true, true, nil
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
		return nil, true, false, err
	}
	if err != nil {
		return nil, true, false, nil
	}
	return u, true, false, nil
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
	// #nosec G124 -- Secure is off only with -insecure (local testing)
	http.SetCookie(w, &http.Cookie{Name: s.cookieName(), Value: "", Path: "/", MaxAge: -1,
		Secure: !s.cfg.Insecure, HttpOnly: true, SameSite: http.SameSiteStrictMode})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
