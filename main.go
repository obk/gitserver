package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
	"rsc.io/qr"
)

const usage = `gitserver - minimal self-hosted git server (web UI, git over SSH, read-only HTTPS clone)

Usage:
  gitserver serve [-listen ADDR] [-base-url URL] [-ssh-host HOST] [-site NAME]
                  [-tls-cert FILE -tls-key FILE] [-trust-proxy [-real-ip-header X-Real-IP]] [-insecure]
  gitserver user add [-admin] [-ssh-key KEY] [-issuer NAME] USER
                                                      create a user (password, TOTP, SSH key)
  gitserver user passwd USER                          set a new password
  gitserver user totp [-issuer NAME] USER             enroll a new TOTP authenticator
  gitserver user admin USER true|false                grant or revoke admin (invites)
  gitserver user key add USER 'ssh-ed25519 AAAA...'   add an SSH public key
  gitserver user key list USER | user key del USER ID
  gitserver user del USER
  gitserver user list
  gitserver invite create [-admin] [-expires 168h] [-base-url URL]
                                                      create a single-use signup link
  gitserver invite list | invite revoke ID
  gitserver repo create [-public] [-desc TEXT] ~OWNER/NAME
  gitserver repo public ~OWNER/NAME | repo private ~OWNER/NAME
  gitserver repo list
  gitserver ssh-keys USER TYPE KEY                    sshd AuthorizedKeysCommand (see deploy/)
  gitserver ssh-serve USER                            forced command for SSH keys
  gitserver backup FILE                               consistent snapshot of the database
  gitserver demo [-listen ADDR]                       throwaway local server with sample data
  gitserver version

Every command accepts -data DIR (default: $GITSERVER_DATA or ./data).
Flags must come before positional arguments.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(os.Args[2:])
	case "user":
		err = cmdUser(os.Args[2:])
	case "ssh-keys":
		err = cmdSSHKeys(os.Args[2:])
	case "ssh-serve":
		err = cmdSSHServe(os.Args[2:])
	case "repo":
		err = cmdRepo(os.Args[2:])
	case "invite":
		err = cmdInvite(os.Args[2:])
	case "demo":
		err = cmdDemo(os.Args[2:])
	case "backup":
		err = cmdBackup(os.Args[2:])
	case "version":
		fmt.Println("gitserver", version)
	case "help", "-h", "-help", "--help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

var version = "dev"

var errUsage = errors.New("invalid arguments (see gitserver help)")

func newFlagSet(name string) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	def := os.Getenv("GITSERVER_DATA")
	if def == "" {
		def = "data"
	}
	return fs, fs.String("data", def, "data directory")
}

func cmdServe(args []string) error {
	fs, data := newFlagSet("serve")
	var cfg Config
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "listen address")
	fs.StringVar(&cfg.BaseURL, "base-url", "", "public base URL of the web UI, e.g. https://git.example.com")
	fs.StringVar(&cfg.SSHHost, "ssh-host", "", "host in SSH clone URLs (default: host of -base-url)")
	fs.StringVar(&cfg.SiteName, "site", "git", "site name shown in the header")
	fs.StringVar(&cfg.TLSCert, "tls-cert", "", "TLS certificate file (serve HTTPS directly)")
	fs.StringVar(&cfg.TLSKey, "tls-key", "", "TLS key file")
	fs.BoolVar(&cfg.TrustProxy, "trust-proxy", false, "trust X-Forwarded-For/-Proto from a reverse proxy")
	fs.StringVar(&cfg.RealIPHeader, "real-ip-header", "", "with -trust-proxy: header holding the client IP, e.g. X-Real-IP")
	fs.BoolVar(&cfg.Insecure, "insecure", false, "allow login over plain HTTP (local testing only)")
	fs.Parse(args)
	if fs.NArg() != 0 {
		return errUsage
	}
	cfg.DataDir = *data
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return errors.New("-tls-cert and -tls-key must be given together")
	}
	if cfg.BaseURL == "" && !cfg.Insecure {
		log.Print("warning: no -base-url; invite links and HTTPS clone URLs will use the Host header of each request")
	}
	if cfg.TLSCert == "" && !cfg.TrustProxy && !cfg.Insecure {
		log.Print("warning: no TLS and no -trust-proxy; session cookies are Secure-only, so login needs HTTPS in front of this server")
	}

	s, err := NewServer(cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		log.Printf("listening on %s (data: %s)", cfg.Listen, cfg.DataDir)
		if cfg.TLSCert != "" {
			errc <- srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Print("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func cmdUser(args []string) error {
	if len(args) < 1 {
		return errUsage
	}
	sub, rest := args[0], args[1:]
	if sub == "key" { // "user key add|list|del ...": flags come after the second word
		if len(rest) == 0 {
			return errUsage
		}
		sub, rest = "key "+rest[0], rest[1:]
	}
	fs, data := newFlagSet("user " + sub)
	admin := fs.Bool("admin", false, "make the user an admin (can manage invites)")
	issuer := fs.String("issuer", "gitserver", "issuer name shown in the authenticator app")
	sshKey := fs.String("ssh-key", "", "SSH public key for the new user (prompted if empty)")
	fs.Parse(rest)
	store, err := openStore(*data)
	if err != nil {
		return err
	}

	if sub == "list" {
		users, err := store.List()
		if err != nil {
			return err
		}
		for _, u := range users {
			role := "user"
			if u.Admin {
				role = "admin"
			}
			fmt.Printf("%-32s %-5s %d SSH key(s)\n", u.Name, role, len(u.SSHKeys))
		}
		return nil
	}

	if keySub, ok := strings.CutPrefix(sub, "key "); ok {
		return cmdUserKey(store, keySub, fs.Args())
	}
	wantArgs := 1
	if sub == "admin" {
		wantArgs = 2
	}
	if fs.NArg() != wantArgs {
		return errUsage
	}
	name := fs.Arg(0)

	switch sub {
	case "add":
		if blockedUserName(name) {
			return fmt.Errorf("%q is a reserved user name", name)
		}
		if !validUserName(name) {
			return errors.New("user names must match [a-z0-9][a-z0-9_-]{0,31}")
		}
		if _, err := store.Get(name); err == nil {
			return errUserExists
		}
		if used, err := store.NameUsed(name); err != nil {
			return err
		} else if used {
			return fmt.Errorf("%q: %w", name, errNameUsed)
		}
		if dir := filepath.Join(*data, "repos", name); ownerDirExists(filepath.Join(*data, "repos"), name) {
			fmt.Fprintf(os.Stderr, "warning: %s exists; the new user %s will own every repository in it.\n", dir, name)
		}
		keyText := *sshKey
		if keyText == "" {
			if keyText, err = prompt("SSH public key (contents of ~/.ssh/id_ed25519.pub): "); err != nil {
				return err
			}
		}
		key, err := parseSSHKey(keyText)
		if err != nil {
			return err
		}
		pw, err := promptNewPassword()
		if err != nil {
			return err
		}
		hash, err := hashPassword(pw)
		if err != nil {
			return err
		}
		box, err := loadSecretBox(*data, store)
		if err != nil {
			return err
		}
		secret, step, err := enrollTOTP(*issuer, name)
		if err != nil {
			return err
		}
		if err := store.Create(&User{Name: name, PasswordHash: hash, TOTPSecret: box.sealTOTP(name, secret), TOTPLast: step, Admin: *admin, SSHKeys: []SSHKey{key}}); err != nil {
			return err
		}
		fmt.Printf("User %s created.\n", name)
	case "passwd":
		if _, err := store.Get(name); err != nil {
			return err
		}
		pw, err := promptNewPassword()
		if err != nil {
			return err
		}
		hash, err := hashPassword(pw)
		if err != nil {
			return err
		}
		if err := store.Update(name, func(u *User) error { u.PasswordHash = hash; return nil }); err != nil {
			return err
		}
		fmt.Println("Password updated.")
	case "totp":
		if _, err := store.Get(name); err != nil {
			return err
		}
		box, err := loadSecretBox(*data, store)
		if err != nil {
			return err
		}
		secret, step, err := enrollTOTP(*issuer, name)
		if err != nil {
			return err
		}
		sealed := box.sealTOTP(name, secret)
		if err := store.Update(name, func(u *User) error { u.TOTPSecret, u.TOTPLast = sealed, step; return nil }); err != nil {
			return err
		}
		fmt.Println("Authenticator updated.")
	case "admin":
		val := fs.Arg(1)
		if val != "true" && val != "false" {
			return errUsage
		}
		return store.Update(name, func(u *User) error { u.Admin = val == "true"; return nil })
	case "del":
		if err := store.Delete(name); err != nil {
			return err
		}
		fmt.Printf("User %s deleted.\n", name)
	default:
		return errUsage
	}
	return nil
}

func cmdBackup(args []string) error {
	fs, data := newFlagSet("backup")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errUsage
	}
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.Backup(fs.Arg(0)); err != nil {
		return err
	}
	fmt.Println("Database saved to", fs.Arg(0))
	return nil
}

func cmdInvite(args []string) error {
	if len(args) < 1 {
		return errUsage
	}
	sub := args[0]
	fs, data := newFlagSet("invite " + sub)
	admin := fs.Bool("admin", false, "the new account becomes an admin")
	expires := fs.Duration("expires", 7*24*time.Hour, "how long the link stays valid")
	baseURL := fs.String("base-url", "", "public base URL, e.g. https://git.example.com")
	fs.Parse(args[1:])
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	switch sub {
	case "create":
		if fs.NArg() != 0 || *expires <= 0 {
			return errUsage
		}
		code, inv, err := store.CreateInvite("cli", *admin, *expires)
		if err != nil {
			return err
		}
		fmt.Println(strings.TrimRight(*baseURL, "/") + "/signup?code=" + code)
		fmt.Fprintf(os.Stderr, "Invite %s expires %s. The link is shown only once.\n", inv.ID, inv.Expires.Local().Format(time.DateTime))
	case "list":
		invites, err := store.Invites()
		if err != nil {
			return err
		}
		now := time.Now()
		for _, i := range invites {
			status := "active"
			switch {
			case i.UsedBy != "":
				status = "used by " + i.UsedBy
			case !i.Usable(now):
				status = "expired"
			}
			role := "user"
			if i.Admin {
				role = "admin"
			}
			fmt.Printf("%-10s %-5s %-16s by %-12s %s\n", i.ID, role, i.Created.Local().Format(time.DateTime)[:16], i.CreatedBy, status)
		}
	case "revoke":
		if fs.NArg() != 1 {
			return errUsage
		}
		return store.RevokeInvite(fs.Arg(0))
	default:
		return errUsage
	}
	return nil
}

func cmdUserKey(store *Store, sub string, args []string) error {
	if len(args) < 1 {
		return errUsage
	}
	name := args[0]
	switch {
	case sub == "list" && len(args) == 1:
		u, err := store.Get(name)
		if err != nil {
			return err
		}
		for _, k := range u.SSHKeys {
			fmt.Printf("%-12s %s %s\n", k.ID, k.Fingerprint, k.Comment)
		}
	case sub == "add" && len(args) == 2:
		key, err := parseSSHKey(args[1])
		if err != nil {
			return err
		}
		if err := store.AddSSHKey(name, key); err != nil {
			return err
		}
		fmt.Println("Added", key.Fingerprint)
	case sub == "del" && len(args) == 2:
		return store.DeleteSSHKey(name, args[1])
	default:
		return errUsage
	}
	return nil
}

func cmdRepo(args []string) error {
	if len(args) < 1 {
		return errUsage
	}
	sub := args[0]
	fs, data := newFlagSet("repo " + sub)
	public := fs.Bool("public", false, "make the repository public (default private)")
	desc := fs.String("desc", "", "description")
	fs.Parse(args[1:])
	reposDir := filepath.Join(*data, "repos")

	switch sub {
	case "list":
		repos, err := listRepos(reposDir, "")
		if err != nil {
			return err
		}
		for _, r := range repos {
			vis := "private"
			if r.Public {
				vis = "public"
			}
			fmt.Printf("%-40s %-7s %s\n", r.FullName(), vis, r.Description)
		}
		return nil
	case "create", "public", "private":
	default:
		return errUsage
	}
	if fs.NArg() != 1 {
		return errUsage
	}
	owner, name, err := parseRepoRef(fs.Arg(0))
	if err != nil {
		return err
	}
	switch sub {
	case "create":
		store, err := openStore(*data)
		if err != nil {
			return err
		}
		if _, err := store.Get(owner); err != nil {
			return fmt.Errorf("no user %q", owner)
		}
		if err := createRepo(reposDir, owner, name, *desc, *public); err != nil {
			return err
		}
		vis := "private"
		if *public {
			vis = "public"
		}
		fmt.Printf("Created %s repository ~%s/%s\n", vis, owner, name)
		return nil
	default:
		return setRepoPublic(reposDir, owner, name, sub == "public")
	}
}

var stdin = bufio.NewReader(os.Stdin)

func prompt(label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	line, err := stdin.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func promptSecret(label string) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return prompt(label)
	}
	fmt.Fprint(os.Stderr, label)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	return string(b), err
}

func promptNewPassword() (string, error) {
	pw, err := promptSecret("Password (min. 12 characters): ")
	if err != nil {
		return "", err
	}
	if n := len([]rune(pw)); n < 12 || len(pw) > 1024 {
		return "", errors.New("password must be 12 to 1024 characters long")
	}
	again, err := promptSecret("Repeat password: ")
	if err != nil {
		return "", err
	}
	if again != pw {
		return "", errors.New("passwords do not match")
	}
	return pw, nil
}

// enrollTOTP shows a new secret as a QR code and requires one valid code
// before returning it. The returned step marks that code as used.
func enrollTOTP(issuer, account string) (secret string, step int64, err error) {
	secret = newTOTPSecret()
	uri := totpURI(issuer, account, secret)
	fmt.Fprintln(os.Stderr, "\nScan this QR code with your authenticator app (Aegis, 2FAS, Google Authenticator, ...):")
	if err := printQR(uri); err != nil {
		return "", 0, err
	}
	fmt.Fprintf(os.Stderr, "Or enter the secret manually: %s\n\n", secret)
	for range 3 {
		code, err := prompt("Enter the 6-digit code to confirm: ")
		if err != nil {
			return "", 0, err
		}
		if step, ok := checkTOTP(secret, code, 0, time.Now()); ok {
			return secret, step, nil
		}
		fmt.Fprintln(os.Stderr, "Invalid code (is your clock correct?).")
	}
	return "", 0, errors.New("TOTP enrollment failed")
}

// printQR draws a QR code with half-block characters, two modules per row,
// with explicit colors so it scans on both dark and light terminals.
func printQR(text string) error {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return err
	}
	const quiet = 2
	black := func(x, y int) bool {
		if x < 0 || y < 0 || x >= code.Size || y >= code.Size {
			return false
		}
		return code.Black(x, y)
	}
	var b strings.Builder
	for y := -quiet; y < code.Size+quiet; y += 2 {
		for x := -quiet; x < code.Size+quiet; x++ {
			fg, bg := "97", "107" // white
			if black(x, y) {
				fg = "30"
			}
			if black(x, y+1) {
				bg = "40"
			}
			fmt.Fprintf(&b, "\x1b[%s;%sm▀", fg, bg)
		}
		b.WriteString("\x1b[0m\n")
	}
	fmt.Fprint(os.Stderr, b.String())
	return nil
}
