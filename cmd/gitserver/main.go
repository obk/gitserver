// Command gitserver is a small self-hosted git server: a web UI, git over
// SSH and read-only HTTPS clone. Run "gitserver help" for the commands.
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
	"rsc.io/qr"

	"go-git-server/internal/account"
	"go-git-server/internal/gitrepo"
	"go-git-server/internal/sshgit"
	"go-git-server/internal/store"
	"go-git-server/internal/web"
)

const usage = `gitserver - minimal self-hosted git server (web UI, git over SSH, read-only HTTPS clone)

Usage:
  gitserver serve [-listen ADDR] [-base-url URL] [-ssh-host HOST] [-site NAME]
                  [-tls-cert FILE -tls-key FILE] [-trust-proxy [-real-ip-header X-Real-IP]] [-insecure]
                  (-listen takes host:port, unix:/path/to.sock, or systemd for socket activation)
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
  gitserver fsck                                      check every repository with git fsck (weekly timer)
  gitserver mirror-sync                               fetch the pull mirrors that are due (timer)
  gitserver demo [-listen ADDR]                       throwaway local server with sample data
  gitserver update [-f] [VERSION]                     on an installed server: download the newest
                                                      release and install it (runs gitserverctl update)
  gitserver version

Every command accepts -data DIR (default: $GITSERVER_DATA or ./data).
Flags must come before positional arguments.
`

func main() {
	if filepath.Base(os.Args[0]) == sshgit.HookName {
		preReceiveHook()
		return
	}
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
	case "fsck":
		err = cmdFsck(os.Args[2:])
	case "mirror-sync":
		err = cmdMirrorSync(os.Args[2:])
	case "update":
		err = cmdUpdate(os.Args[2:])
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
	var cfg web.Config
	fs.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "listen address: host:port, unix:/path/to.sock, or systemd (socket activation)")
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

	s, err := web.NewServer(cfg)
	if err != nil {
		return err
	}
	ln, err := listen(cfg.Listen)
	if err != nil {
		return err
	}
	srv := &http.Server{
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
			errc <- srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		} else {
			errc <- srv.Serve(ln)
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
	st, err := store.Open(*data)
	if err != nil {
		return err
	}

	if sub == "list" {
		users, err := st.List()
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
		return cmdUserKey(st, keySub, fs.Args())
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
		if account.BlockedUserName(name) {
			return fmt.Errorf("%q is a reserved user name", name)
		}
		if !account.ValidUserName(name) {
			return errors.New("user names must match [a-z0-9][a-z0-9_-]{0,31}")
		}
		if _, err := st.Get(name); err == nil {
			return store.ErrUserExists
		}
		if used, err := st.NameUsed(name); err != nil {
			return err
		} else if used {
			return fmt.Errorf("%q: %w", name, store.ErrNameUsed)
		}
		if dir := filepath.Join(*data, "repos", name); gitrepo.OwnerDirExists(filepath.Join(*data, "repos"), name) {
			fmt.Fprintf(os.Stderr, "warning: %s exists; the new user %s will own every repository in it.\n", dir, name)
		}
		keyText := *sshKey
		if keyText == "" {
			if keyText, err = prompt("SSH public key (contents of ~/.ssh/id_ed25519.pub): "); err != nil {
				return err
			}
		}
		key, err := sshgit.ParseKey(keyText)
		if err != nil {
			return err
		}
		pw, err := promptNewPassword()
		if err != nil {
			return err
		}
		hash, err := account.HashPassword(pw)
		if err != nil {
			return err
		}
		box, err := store.LoadSecretBox(*data, st)
		if err != nil {
			return err
		}
		secret, step, err := enrollTOTP(*issuer, name)
		if err != nil {
			return err
		}
		if err := st.Create(&store.User{Name: name, PasswordHash: hash, TOTPSecret: box.SealTOTP(name, secret), TOTPLast: step, Admin: *admin, SSHKeys: []store.SSHKey{key}}); err != nil {
			return err
		}
		role := ""
		if *admin {
			role = "admin"
		}
		cliAudit(st, "user created", name, role)
		fmt.Printf("User %s created.\n", name)
	case "passwd":
		if _, err := st.Get(name); err != nil {
			return err
		}
		pw, err := promptNewPassword()
		if err != nil {
			return err
		}
		hash, err := account.HashPassword(pw)
		if err != nil {
			return err
		}
		if err := st.Update(name, func(u *store.User) error { u.PasswordHash = hash; return nil }); err != nil {
			return err
		}
		cliAudit(st, "password changed", name, "")
		fmt.Println("Password updated.")
	case "totp":
		if _, err := st.Get(name); err != nil {
			return err
		}
		box, err := store.LoadSecretBox(*data, st)
		if err != nil {
			return err
		}
		secret, step, err := enrollTOTP(*issuer, name)
		if err != nil {
			return err
		}
		sealed := box.SealTOTP(name, secret)
		if err := st.Update(name, func(u *store.User) error { u.TOTPSecret, u.TOTPLast = sealed, step; return nil }); err != nil {
			return err
		}
		cliAudit(st, "new authenticator", name, "")
		fmt.Println("Authenticator updated.")
	case "admin":
		val := fs.Arg(1)
		if val != "true" && val != "false" {
			return errUsage
		}
		if err := st.Update(name, func(u *store.User) error { u.Admin = val == "true"; return nil }); err != nil {
			return err
		}
		action := "admin rights given"
		if val == "false" {
			action = "admin rights removed"
		}
		cliAudit(st, action, name, "")
	case "del":
		if err := st.Delete(name); err != nil {
			return err
		}
		cliAudit(st, "user deleted", name, "")
		fmt.Printf("User %s deleted.\n", name)
	default:
		return errUsage
	}
	return nil
}

// cmdSSHKeys: gitserver ssh-keys [-ssh-user git] USER TYPE KEY (see sshgit.AuthorizedKeys).
func cmdSSHKeys(args []string) error {
	fs, data := newFlagSet("ssh-keys")
	sshUser := fs.String("ssh-user", "git", "system user that git connects as")
	fs.Parse(args)
	return sshgit.AuthorizedKeys(os.Stdout, *data, *sshUser, fs.Args())
}

// cmdSSHServe: gitserver ssh-serve USER, the forced command (see sshgit.Serve).
func cmdSSHServe(args []string) error {
	fs, data := newFlagSet("ssh-serve")
	key := fs.String("key", "", "ID of the SSH key used (recorded as its last use)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("invalid forced command")
	}
	return sshgit.Serve(*data, fs.Arg(0), *key)
}

func cmdBackup(args []string) error {
	fs, data := newFlagSet("backup")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errUsage
	}
	st, err := store.Open(*data)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Backup(fs.Arg(0)); err != nil {
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
	st, err := store.Open(*data)
	if err != nil {
		return err
	}
	switch sub {
	case "create":
		if fs.NArg() != 0 || *expires <= 0 {
			return errUsage
		}
		code, inv, err := st.CreateInvite("cli", *admin, *expires)
		if err != nil {
			return err
		}
		detail := "id " + inv.ID + ", expires " + inv.Expires.UTC().Format("2006-01-02 15:04 UTC")
		if *admin {
			detail += ", makes an admin"
		}
		cliAudit(st, "invite created", "", detail)
		fmt.Println(strings.TrimRight(*baseURL, "/") + "/signup?code=" + code)
		fmt.Fprintf(os.Stderr, "Invite %s expires %s. The link is shown only once.\n", inv.ID, inv.Expires.Local().Format(time.DateTime))
	case "list":
		invites, err := st.Invites("")
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
		if err := st.RevokeInvite(fs.Arg(0), ""); err != nil {
			return err
		}
		cliAudit(st, "invite revoked", "", "id "+fs.Arg(0))
	default:
		return errUsage
	}
	return nil
}

func cmdUserKey(st *store.Store, sub string, args []string) error {
	if len(args) < 1 {
		return errUsage
	}
	name := args[0]
	switch {
	case sub == "list" && len(args) == 1:
		u, err := st.Get(name)
		if err != nil {
			return err
		}
		for _, k := range u.SSHKeys {
			fmt.Printf("%-12s %s %s\n", k.ID, k.Fingerprint, k.Comment)
		}
	case sub == "add" && len(args) == 2:
		key, err := sshgit.ParseKey(args[1])
		if err != nil {
			return err
		}
		if err := st.AddSSHKey(name, key); err != nil {
			return err
		}
		cliAudit(st, "SSH key added", name, key.Fingerprint)
		fmt.Println("Added", key.Fingerprint)
	case sub == "del" && len(args) == 2:
		if err := st.DeleteSSHKey(name, args[1]); err != nil {
			return err
		}
		cliAudit(st, "SSH key deleted", name, "id "+args[1])
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
		repos, err := gitrepo.List(reposDir, "")
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
	owner, name, err := gitrepo.ParseRef(fs.Arg(0))
	if err != nil {
		return err
	}
	switch sub {
	case "create":
		st, err := store.Open(*data)
		if err != nil {
			return err
		}
		if _, err := st.Get(owner); err != nil {
			return fmt.Errorf("no user %q", owner)
		}
		if err := gitrepo.Create(reposDir, owner, name, *desc, *public); err != nil {
			return err
		}
		vis := "private"
		if *public {
			vis = "public"
		}
		fmt.Printf("Created %s repository ~%s/%s\n", vis, owner, name)
		return nil
	default:
		return gitrepo.SetPublic(reposDir, owner, name, sub == "public")
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
func enrollTOTP(issuer, name string) (secret string, step int64, err error) {
	secret = account.NewTOTPSecret()
	uri := account.TOTPURI(issuer, name, secret)
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
		if step, ok := account.CheckTOTP(secret, code, 0, time.Now()); ok {
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

// cliAudit records a change made with the command line in the audit log
// that admins see on the web. The actor names the sudo user, if any
// (gitserverctl keeps SUDO_USER); it is never a user name, which can't
// contain spaces. A failure is only a warning: the change itself is done.
func cliAudit(st *store.Store, action, target, detail string) {
	actor := "command line"
	if u := os.Getenv("SUDO_USER"); u != "" && u != "root" {
		actor += " (sudo " + u + ")"
	}
	if err := st.Audit(store.AuditEntry{Actor: actor, Action: action, Target: target, Detail: detail}); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not write the audit log:", err)
	}
}

// gitserverctl is the admin helper the installer puts on the server.
var gitserverctl = "/usr/local/sbin/gitserverctl"

// cmdUpdate hands "gitserver update" to "gitserverctl update", which
// downloads and installs a release as root.
func cmdUpdate(args []string) error {
	if _, err := os.Stat(gitserverctl); err != nil {
		return errors.New("gitserver update works on a server set up with deploy/install.sh (" + gitserverctl + " is missing); to update by hand, see Updating in WIKI.md")
	}
	return syscall.Exec(gitserverctl, append([]string{gitserverctl, "update"}, args...), os.Environ())
}

// preReceiveHook runs when git starts this program as the pre-receive hook
// of a push (see sshgit.hookDir). Its output goes to the pusher; failing
// refuses the push.
func preReceiveHook() {
	dir, err := sshgit.HookRepoDir()
	var gitPath string
	if err == nil {
		gitPath, err = exec.LookPath("git")
	}
	if err == nil {
		err = sshgit.PreReceive(gitPath, dir, os.Stdin, os.Stderr)
	}
	if err != nil {
		if !errors.Is(err, sshgit.ErrProtected) {
			fmt.Fprintln(os.Stderr, "gitserver: checking protected branches failed:", err)
		}
		os.Exit(1)
	}
}
