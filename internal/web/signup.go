package web

import (
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"rsc.io/qr"

	"go-git-server/internal/account"
	"go-git-server/internal/gitrepo"
	"go-git-server/internal/sshgit"
	"go-git-server/internal/store"
)

// Signup is invite-only and takes two steps:
//
//  1. /signup?code=...  choose a username, password and SSH key
//  2. scan the TOTP QR code and confirm with one code
//
// Between the steps the account lives only in memory (pendingSignups) and
// the invite stays unused; it is consumed atomically when the user is created.

const (
	pendingSignupTTL = 15 * time.Minute
	maxPendingSignup = 1000
)

var errBusy = errors.New("too many signups in progress; try again in a few minutes")

type pendingSignup struct {
	inviteHash string
	user       string
	pwHash     string
	secret     string
	key        store.SSHKey
	expires    time.Time
}

type pendingSignups struct {
	mu sync.Mutex
	m  map[string]*pendingSignup // keyed by SHA-256 of the form token
}

func newPendingSignups() *pendingSignups {
	p := &pendingSignups{m: make(map[string]*pendingSignup)}
	go func() {
		for range time.Tick(pendingSignupTTL) {
			p.mu.Lock()
			for k, v := range p.m {
				if time.Now().After(v.expires) {
					delete(p.m, k)
				}
			}
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *pendingSignups) add(ps *pendingSignup) (string, error) {
	token := account.RandomToken(32)
	ps.expires = time.Now().Add(pendingSignupTTL)
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.m) >= maxPendingSignup {
		return "", errBusy
	}
	p.m[account.HashToken(token)] = ps
	return token, nil
}

func (p *pendingSignups) get(token string) (*pendingSignup, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ps, ok := p.m[account.HashToken(token)]
	if !ok || time.Now().After(ps.expires) {
		return nil, false
	}
	return ps, true
}

func (p *pendingSignups) delete(token string) {
	p.mu.Lock()
	delete(p.m, account.HashToken(token))
	p.mu.Unlock()
}

type signupData struct {
	Code     string
	Invalid  bool
	Error    string
	Name     string
	Key      string
	Step     int
	Token    string
	QR       template.HTML
	Secret   string
	Username string
}

func (s *Server) handleSignupForm(w http.ResponseWriter, r *http.Request) {
	if currentUser(r) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	code := r.URL.Query().Get("code")
	data := signupData{Code: code, Step: 1}
	if code == "" {
		data.Invalid = true
		s.render(w, http.StatusOK, "signup", s.newPage(r, "Sign up", data))
		return
	}
	ip := s.clientIP(r)
	if s.limiter.blocked(ipKey(ip)) {
		s.error(w, r, http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	if _, err := s.store.LookupInvite(code); err != nil {
		s.limiter.fail(ipKey(ip))
		data.Invalid = true
		data.Error = "This invite link is invalid, expired or has already been used."
		s.render(w, http.StatusNotFound, "signup", s.newPage(r, "Sign up", data))
		return
	}
	s.render(w, http.StatusOK, "signup", s.newPage(r, "Sign up", data))
}

func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	code := r.PostFormValue("code")
	name := strings.TrimSpace(r.PostFormValue("username"))
	pw, pw2 := r.PostFormValue("password"), r.PostFormValue("password2")
	keyText := r.PostFormValue("key")
	data := signupData{Code: code, Name: name, Key: keyText, Step: 1}
	fail := func(status int, msg string) {
		data.Error = msg
		s.render(w, status, "signup", s.newPage(r, "Sign up", data))
	}

	ip := s.clientIP(r)
	if s.limiter.blocked(ipKey(ip)) {
		fail(http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	if _, err := s.store.LookupInvite(code); err != nil {
		s.limiter.fail(ipKey(ip))
		data.Invalid = true
		fail(http.StatusNotFound, "This invite link is invalid, expired or has already been used.")
		return
	}
	if !account.UserNameRe.MatchString(name) {
		fail(http.StatusBadRequest, "Usernames are 1-32 characters: lowercase letters, digits, - and _, starting with a letter or digit.")
		return
	}
	if account.BlockedUserName(name) {
		fail(http.StatusBadRequest, "That username is reserved. Please choose another one.")
		return
	}
	if !account.ValidUserName(name) {
		fail(http.StatusBadRequest, "Usernames are 1-32 characters: lowercase letters, digits, - and _, starting with a letter or digit.")
		return
	}
	if used, err := s.store.NameUsed(name); err != nil || used || gitrepo.OwnerDirExists(s.reposDir, name) {
		fail(http.StatusBadRequest, "That username is taken.")
		return
	}
	if err := checkNewPassword(pw, pw2); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	key, err := sshgit.ParseKey(keyText)
	if err != nil {
		fail(http.StatusBadRequest, "SSH key: "+capitalize(err.Error())+".")
		return
	}
	if _, err := s.store.UserByKey(key.Fingerprint); err == nil {
		fail(http.StatusBadRequest, "SSH key: "+capitalize(store.ErrKeyInUse.Error())+".")
		return
	}
	hash, err := account.HashPassword(pw)
	if errors.Is(err, account.ErrHashBusy) {
		fail(http.StatusServiceUnavailable, capitalize(err.Error())+".")
		return
	}
	if err != nil {
		fail(http.StatusInternalServerError, "Internal error.")
		return
	}
	ps := &pendingSignup{inviteHash: account.HashToken(code), user: name, pwHash: hash, secret: account.NewTOTPSecret(), key: key}
	token, err := s.pending.add(ps)
	if err != nil {
		fail(http.StatusServiceUnavailable, capitalize(err.Error())+".")
		return
	}
	s.renderTOTPStep(w, r, http.StatusOK, token, ps, "")
}

func (s *Server) renderTOTPStep(w http.ResponseWriter, r *http.Request, status int, token string, ps *pendingSignup, errMsg string) {
	svg, err := qrSVG(account.TOTPURI(s.cfg.SiteName, ps.user, ps.secret))
	if err != nil {
		s.error(w, r, http.StatusInternalServerError, "Could not create QR code.")
		return
	}
	data := signupData{Step: 2, Token: token, QR: svg, Secret: ps.secret, Username: ps.user, Error: errMsg}
	s.render(w, status, "signup", s.newPage(r, "Sign up", data))
}

func (s *Server) handleSignupConfirm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	token := r.PostFormValue("token")
	ip := s.clientIP(r)
	if s.limiter.blocked(ipKey(ip)) {
		s.error(w, r, http.StatusTooManyRequests, "Too many failed attempts. Try again later.")
		return
	}
	ps, ok := s.pending.get(token)
	if !ok {
		s.error(w, r, http.StatusBadRequest, "Your signup session expired. Open the invite link again.")
		return
	}
	step, ok := account.CheckTOTP(ps.secret, r.PostFormValue("totp"), 0, time.Now())
	if !ok {
		s.limiter.fail(ipKey(ip))
		s.renderTOTPStep(w, r, http.StatusBadRequest, token, ps, "That code is not valid. Check that your device's clock is correct.")
		return
	}
	if gitrepo.OwnerDirExists(s.reposDir, ps.user) {
		s.pending.delete(token)
		s.error(w, r, http.StatusConflict, "That username was taken in the meantime. Open the invite link again.")
		return
	}
	u := &store.User{Name: ps.user, PasswordHash: ps.pwHash, TOTPSecret: s.box.SealTOTP(ps.user, ps.secret), TOTPLast: step, SSHKeys: []store.SSHKey{ps.key}}
	if err := s.store.RedeemInvite(ps.inviteHash, u); err != nil {
		s.pending.delete(token)
		msg := "This invite link is invalid, expired or has already been used."
		switch {
		case errors.Is(err, store.ErrUserExists), errors.Is(err, store.ErrNameUsed):
			msg = "That username was taken in the meantime. Open the invite link again."
		case errors.Is(err, store.ErrKeyInUse):
			msg = "That SSH key was registered to another account in the meantime. Open the invite link again."
		}
		s.error(w, r, http.StatusConflict, msg)
		return
	}
	s.pending.delete(token)
	log.Printf("signup user=%q invited_by=%q ip=%s", u.Name, u.InvitedBy, ip)
	detail := "invited by " + u.InvitedBy
	if u.Admin {
		detail += ", admin"
	}
	s.auditAs(r, u.Name, "signed up", u.Name, detail)
	id := s.startSession(w, r, u, "")
	s.recordLogin(id, u.Name, ip, r.UserAgent(), "signup")
	// Every new account starts with recovery codes, shown once.
	if err := s.newRecoveryCodes(u.Name, id); err != nil {
		log.Printf("signup: recovery codes for user=%q: %v", u.Name, err)
		http.Redirect(w, r, "/settings/2fa", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings/2fa?welcome=1", http.StatusSeeOther)
}

// qrSVG renders text as an inline SVG QR code (allowed by the CSP, unlike
// data: images or scripts).
func qrSVG(text string) (template.HTML, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	const quiet = 4
	n := code.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" class="qr" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="TOTP QR code">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/><path fill="#000" d="`, n, n)
	for y := 0; y < code.Size; y++ {
		for x := 0; x < code.Size; x++ {
			if code.Black(x, y) {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	// #nosec G203 -- SVG drawn from the QR code's modules; no input text
	return template.HTML(b.String()), nil
}
