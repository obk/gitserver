package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	demoUser     = "demo"
	demoPassword = "demo-password"
)

// cmdDemo runs a throwaway server on localhost with sample repositories and
// a ready-made admin account. Current TOTP codes are printed to the terminal
// so no authenticator app is needed. Everything is deleted on exit.
func cmdDemo(args []string) error {
	fs, _ := newFlagSet("demo")
	listen := fs.String("listen", "127.0.0.1:8080", "listen address (keep it on localhost)")
	fs.Parse(args)

	dir, err := os.MkdirTemp("", "gitserver-demo-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	store, err := openStore(dir)
	if err != nil {
		return err
	}
	hash, err := hashPassword(demoPassword)
	if err != nil {
		return err
	}
	secret := newTOTPSecret()
	edPub, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		return err
	}
	sshPub, err := ssh.NewPublicKey(edPub)
	if err != nil {
		return err
	}
	key, err := parseSSHKey(strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " demo-key")
	if err != nil {
		return err
	}
	box, err := loadSecretBox(dir, store)
	if err != nil {
		return err
	}
	if err := store.Create(&User{Name: demoUser, PasswordHash: hash, TOTPSecret: box.sealTOTP(demoUser, secret), Admin: true, SSHKeys: []SSHKey{key}}); err != nil {
		return err
	}
	if err := seedDemoRepos(dir); err != nil {
		return fmt.Errorf("creating sample repositories: %w", err)
	}
	inviteCode, _, err := store.CreateInvite(demoUser, false, 24*time.Hour)
	if err != nil {
		return err
	}
	fakeSSH, err := writeDemoSSH(dir)
	if err != nil {
		return err
	}

	cfg := Config{DataDir: dir, Listen: *listen, SiteName: "demo", Insecure: true, BaseURL: "http://" + *listen, SSHHost: "demo"}
	s, err := NewServer(cfg)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: *listen, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	fmt.Printf(`
gitserver demo running at http://%[1]s   (Ctrl-C to stop; all data is deleted)

  Log in:       username %[2]s, password %[3]s, code printed below
  Try signup:   http://%[1]s/signup?code=%[4]s
                (needs any SSH public key, e.g. the contents of ~/.ssh/id_ed25519.pub)

  Git over SSH is simulated locally (no sshd needed). As user %[2]s:
    export GIT_SSH_COMMAND=%[5]s
    git clone git@demo:~demo/hello
    git clone git@demo:~demo/secret-notes

`, *listen, demoUser, demoPassword, inviteCode, fakeSSH)

	totpKey, _ := b32.DecodeString(secret)
	showCode := func() {
		step := time.Now().Unix() / totpPeriod
		left := totpPeriod - time.Now().Unix()%totpPeriod
		fmt.Printf("  TOTP code for %s: %s  (valid %2ds)\n", demoUser, hotp(totpKey, uint64(step)), left)
	}
	showCode()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-errc:
			return err
		case <-ctx.Done():
			fmt.Println("\nstopping, removing", dir)
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		case now := <-tick.C:
			if now.Unix()%totpPeriod == 0 {
				showCode()
			}
		}
	}
}

// writeDemoSSH creates a stand-in for ssh that runs gitserver's forced
// command directly, the way sshd would for the demo user's key.
func writeDemoSSH(dir string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "ssh")
	script := fmt.Sprintf("#!/bin/sh\n# Demo only: acts like sshd's forced command for user %s.\n"+
		"for last; do :; done\nSSH_ORIGINAL_COMMAND=\"$last\" exec '%s' ssh-serve -data '%s' %s\n", demoUser, exe, dir, demoUser)
	return path, os.WriteFile(path, []byte(script), 0o755)
}

func seedDemoRepos(dataDir string) error {
	reposDir := filepath.Join(dataDir, "repos")
	work := filepath.Join(dataDir, "work")
	if err := createRepo(reposDir, demoUser, "hello", "A small public example project", true); err != nil {
		return err
	}
	if err := createRepo(reposDir, demoUser, "secret-notes", "Only the owner can see this", false); err != nil {
		return err
	}

	git := func(dir string, date string, args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
			"GIT_AUTHOR_NAME=Demo User", "GIT_AUTHOR_EMAIL=demo@example.com",
			"GIT_COMMITTER_NAME=Demo User", "GIT_COMMITTER_EMAIL=demo@example.com",
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %v: %s", args, err, out)
		}
		return nil
	}
	type change struct {
		files map[string]string
		msg   string
		ago   time.Duration
	}
	build := func(name string, changes []change, extra func(dir string) error) error {
		dir := filepath.Join(work, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := git(dir, time.Now().Format(time.RFC3339), "init", "-q", "-b", "main"); err != nil {
			return err
		}
		for _, c := range changes {
			for path, content := range c.files {
				p := filepath.Join(dir, path)
				os.MkdirAll(filepath.Dir(p), 0o755)
				if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
					return err
				}
			}
			date := time.Now().Add(-c.ago).Format(time.RFC3339)
			if err := git(dir, date, "add", "-A"); err != nil {
				return err
			}
			if err := git(dir, date, "commit", "-q", "-m", c.msg); err != nil {
				return err
			}
		}
		if extra != nil {
			if err := extra(dir); err != nil {
				return err
			}
		}
		return git(dir, time.Now().Format(time.RFC3339), "push", "-q", "--all", "--follow-tags", repoDir(reposDir, demoUser, name))
	}

	day := 24 * time.Hour
	err := build("hello", []change{
		{map[string]string{
			"README.md": "# hello\n\nA tiny example project served by **gitserver**.\n\n" +
				"## Usage\n\n```sh\ngo run .\n```\n\n| Feature | Status |\n|---|---|\n| Markdown | yes |\n| Tables | yes |\n\n" +
				"- [x] public repositories\n- [x] private repositories\n- [ ] world domination\n\n" +
				"<script>alert('raw HTML is not rendered')</script>\n",
			"go.mod":  "module example.com/hello\n\ngo 1.22\n",
			"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello, world\")\n}\n",
			"LICENSE": "MIT License\n\nCopyright (c) Demo User\n",
		}, "Initial commit", 40 * day},
		{map[string]string{
			"greet/greet.go": "package greet\n\n// Hello returns a greeting for name.\nfunc Hello(name string) string {\n\treturn \"hello, \" + name\n}\n",
			"main.go":        "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/hello/greet\"\n)\n\nfunc main() {\n\tfmt.Println(greet.Hello(\"world\"))\n}\n",
		}, "Move greeting into its own package\n\nThis makes it easier to test.", 12 * day},
		{map[string]string{
			"greet/greet_test.go": "package greet\n\nimport \"testing\"\n\nfunc TestHello(t *testing.T) {\n\tif got := Hello(\"x\"); got != \"hello, x\" {\n\t\tt.Fatal(got)\n\t}\n}\n",
		}, "Add a test for Hello", 3 * time.Hour},
	}, func(dir string) error {
		date := time.Now().Format(time.RFC3339)
		if err := git(dir, date, "tag", "-a", "v0.1.0", "-m", "First release"); err != nil {
			return err
		}
		return git(dir, date, "branch", "experimental")
	})
	if err != nil {
		return err
	}
	return build("secret-notes", []change{
		{map[string]string{"README.md": "# secret notes\n\nYou can only see this because you logged in.\n"}, "Start notes", 2 * day},
	}, nil)
}
