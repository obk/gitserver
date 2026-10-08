package main

import (
	"crypto/rsa"
	"encoding/base64"
	"errors"
	"fmt"
	"log/syslog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// Git over SSH uses the system's OpenSSH server and a dedicated "git" user:
//
//	sshd ── AuthorizedKeysCommand: gitserver ssh-keys %u %t %k
//	        prints `restrict,command="gitserver ssh-serve USER" KEY` for a known key
//	     └─ runs the forced command: gitserver ssh-serve USER
//	        checks SSH_ORIGINAL_COMMAND and permissions, then execs git
//
// No shell, no forwarding, no PTY: "restrict" disables everything else.

var allowedKeyTypes = map[string]bool{
	ssh.KeyAlgoED25519:    true,
	ssh.KeyAlgoSKED25519:  true,
	ssh.KeyAlgoECDSA256:   true,
	ssh.KeyAlgoECDSA384:   true,
	ssh.KeyAlgoECDSA521:   true,
	ssh.KeyAlgoSKECDSA256: true,
	ssh.KeyAlgoRSA:        true, // at least 2048 bits, checked below
}

// parseSSHKey validates one public key in authorized_keys format.
func parseSSHKey(text string) (SSHKey, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return SSHKey{}, errors.New("paste your public key (for example the contents of ~/.ssh/id_ed25519.pub)")
	}
	if strings.Contains(text, "PRIVATE KEY") {
		return SSHKey{}, errors.New("that is a PRIVATE key; never share it. Paste the .pub file instead")
	}
	if strings.Count(text, "\n") > 0 {
		return SSHKey{}, errors.New("paste exactly one key")
	}
	pub, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(text))
	if err != nil || len(rest) > 0 {
		return SSHKey{}, errors.New("this does not look like an SSH public key")
	}
	if len(options) > 0 {
		return SSHKey{}, errors.New("remove the options in front of the key")
	}
	if !allowedKeyTypes[pub.Type()] {
		return SSHKey{}, fmt.Errorf("key type %s is not allowed; use ed25519 (ssh-keygen -t ed25519)", pub.Type())
	}
	if pub.Type() == ssh.KeyAlgoRSA {
		ck, ok := pub.(ssh.CryptoPublicKey)
		if !ok {
			return SSHKey{}, errors.New("invalid RSA key")
		}
		if rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey); !ok || rk.N.BitLen() < 2048 {
			return SSHKey{}, errors.New("RSA keys must have at least 2048 bits; ed25519 is recommended")
		}
	}
	comment = strings.TrimSpace(comment)
	if !utf8.ValidString(comment) || strings.ContainsFunc(comment, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		comment = ""
	}
	if utf8.RuneCountInString(comment) > 100 {
		comment = string([]rune(comment)[:100])
	}
	return SSHKey{
		ID:          randomToken(9),
		Key:         strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
		Fingerprint: ssh.FingerprintSHA256(pub),
		Comment:     comment,
		Added:       time.Now().UTC(),
	}, nil
}

// cmdSSHKeys is sshd's AuthorizedKeysCommand:
//
//	AuthorizedKeysCommand /usr/local/bin/gitserver ssh-keys -data DIR %u %t %k
//
// It prints a restricted authorized_keys line if the key belongs to a user.
func cmdSSHKeys(args []string) error {
	fs, data := newFlagSet("ssh-keys")
	sshUser := fs.String("ssh-user", "git", "system user that git connects as")
	fs.Parse(args)
	if fs.NArg() != 3 || fs.Arg(0) != *sshUser {
		return nil // not our user: print nothing, so sshd denies the key
	}
	blob, err := base64.StdEncoding.DecodeString(fs.Arg(2))
	if err != nil {
		return nil
	}
	pub, err := ssh.ParsePublicKey(blob)
	if err != nil || pub.Type() != fs.Arg(1) || !allowedKeyTypes[pub.Type()] {
		return nil
	}
	store, err := openStore(*data)
	if err != nil {
		return err
	}
	u, err := store.UserByKey(ssh.FingerprintSHA256(pub))
	if err != nil {
		return nil
	}
	line, err := authorizedKeyLine(*data, u.Name, pub)
	if err != nil {
		return err
	}
	fmt.Println(line)
	return nil
}

var safePathRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

func authorizedKeyLine(dataDir, user string, pub ssh.PublicKey) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	data, err := filepath.Abs(dataDir)
	if err != nil {
		return "", err
	}
	if !safePathRe.MatchString(exe) || !safePathRe.MatchString(data) || !userNameRe.MatchString(user) {
		return "", errors.New("unsafe characters in binary path, data path or user name")
	}
	cmd := fmt.Sprintf("%s ssh-serve -data %s %s", exe, data, user)
	return fmt.Sprintf(`restrict,command="%s" %s`, cmd, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))), nil
}

// Limits for git over SSH, so one account cannot fill the disk or the CPU.
const (
	maxPushSize   = 1 << 30 // receive.maxInputSize: the largest pack one push may send
	minFreeDisk   = 1 << 30 // pushes are refused when less disk space is left
	maxGitPerUser = 4       // git operations one user may run at the same time
)

var errBusyUser = fmt.Errorf("too many git operations of yours are running (at most %d at once); try again when one has finished", maxGitPerUser)

// acquireUserSlot takes one of the user's maxGitPerUser lock files and
// returns its descriptor, or ok false if all are taken. ssh-serve processes
// are separate, so the count lives in flock(2) locks. The descriptor has no
// close-on-exec flag (syscall.Open, unlike os.OpenFile, does not set it), so
// git inherits it: the lock is held until git and its children exit, even
// if they crash.
func acquireUserSlot(dataDir, user string) (fd int, ok bool, err error) {
	dir := filepath.Join(dataDir, "locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return -1, false, err
	}
	for i := range maxGitPerUser {
		fd, err := syscall.Open(filepath.Join(dir, fmt.Sprintf("ssh-%s.%d", user, i)), syscall.O_RDWR|syscall.O_CREAT, 0o600)
		if err != nil {
			return -1, false, err
		}
		if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return fd, true, nil
		}
		syscall.Close(fd)
	}
	return -1, false, nil
}

// freeDisk returns the bytes available to unprivileged users on dir's filesystem.
func freeDisk(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}

var gitServices = map[string]string{
	"git-upload-pack":    "upload-pack",
	"git-upload-archive": "upload-archive",
	"git-receive-pack":   "receive-pack",
}

// sshRepoArgRe matches the quoted path git sends, e.g. '~obk/example.git'.
var sshRepoArgRe = regexp.MustCompile(`^'/?~([a-z0-9][a-z0-9_-]{0,31})/([A-Za-z0-9][A-Za-z0-9._-]{0,99}?)(\.git)?/?'$`)

// parseSSHCommand splits SSH_ORIGINAL_COMMAND into git service, owner and repo.
func parseSSHCommand(cmd string) (service, owner, repo string, err error) {
	verb, arg, ok := strings.Cut(strings.TrimSpace(cmd), " ")
	service, known := gitServices[verb]
	if !ok || !known {
		return "", "", "", errors.New("only git clone, fetch, pull, push and archive are supported")
	}
	m := sshRepoArgRe.FindStringSubmatch(strings.TrimSpace(arg))
	if m == nil {
		return "", "", "", errors.New("invalid repository path; use git@HOST:~user/repo")
	}
	return service, m[1], m[2], nil
}

// cmdSSHServe is the forced command for every SSH key. It never runs a shell.
func cmdSSHServe(args []string) error {
	fs, data := newFlagSet("ssh-serve")
	fs.Parse(args)
	if fs.NArg() != 1 || !userNameRe.MatchString(fs.Arg(0)) {
		return errors.New("invalid forced command")
	}
	name := fs.Arg(0)
	audit := func(format string, a ...any) {
		if w, err := syslog.New(syslog.LOG_AUTH|syslog.LOG_INFO, "gitserver-ssh"); err == nil {
			fmt.Fprintf(w, format, a...)
			w.Close()
		}
	}

	store, err := openStore(*data)
	if err != nil {
		return err
	}
	u, err := store.Get(name)
	if err != nil {
		return errors.New("this key is not registered to any account")
	}
	orig := os.Getenv("SSH_ORIGINAL_COMMAND")
	if orig == "" {
		fmt.Fprintf(os.Stderr, "Hi ~%s! You've successfully authenticated, but there is no shell access.\n", u.Name)
		os.Exit(1)
	}
	service, owner, repoName, err := parseSSHCommand(orig)
	if err != nil {
		audit("rejected command user=%s cmd=%q", u.Name, orig)
		return err
	}
	repo, err := loadRepo(filepath.Join(*data, "repos"), owner, repoName)
	write := service == "receive-pack"
	if err != nil || !repo.canRead(u) || (write && !repo.canWrite(u)) {
		// Same answer for missing, private and read-only repos.
		audit("denied user=%s service=%s repo=~%s/%s", u.Name, service, owner, repoName)
		return fmt.Errorf("repository ~%s/%s not found or access denied", owner, repoName)
	}
	if _, ok, err := acquireUserSlot(*data, u.Name); err != nil {
		return err
	} else if !ok {
		audit("busy user=%s service=%s repo=%s", u.Name, service, repo.FullName())
		return errBusyUser
	}
	if write {
		if free, err := freeDisk(repo.Dir); err == nil && free < minFreeDisk {
			audit("disk full user=%s repo=%s free=%d", u.Name, repo.FullName(), free)
			return errors.New("the server is almost out of disk space, so pushing is disabled for now; please tell the administrator")
		}
	}
	audit("user=%s service=%s repo=%s", u.Name, service, repo.FullName())

	gitPath, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	env := []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=" + os.Getenv("HOME"), "GIT_TERMINAL_PROMPT=0"}
	// Protocol v2 is not offered: its upload-pack serves any object by
	// hash, including commits no branch or tag points to any more (a
	// force-pushed secret). v0/v1 only serve what the refs reach.
	if p := os.Getenv("GIT_PROTOCOL"); p == "version=1" {
		env = append(env, "GIT_PROTOCOL="+p)
	}
	argv := []string{"git", service}
	switch service {
	case "upload-pack":
		argv = append(argv, "--strict")
	case "receive-pack":
		argv = []string{"git", "-c", fmt.Sprintf("receive.maxInputSize=%d", maxPushSize), service}
	}
	argv = append(argv, repo.Dir)
	return syscall.Exec(gitPath, argv, env)
}
