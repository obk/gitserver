# gitserver

A small, self-hosted git server written in Go, styled after sourcehut's [git.sr.ht](https://git.sr.ht).

It's a single program that gives you:

- a **web UI** to browse repositories: summary with README, file tree, log, refs, commits with diffs, and syntax highlighting
- **git over SSH** for pushing and private repos: `git@git.example.com:~you/project`
- **read-only HTTPS clone** for public repos: `https://git.example.com/~you/project`
- **accounts with mandatory two-factor login** (password + authenticator app), invite-only signup, and SSH keys
- a ready-made **VPS installer** that sets up Caddy (automatic HTTPS), Anubis (bot protection), OpenSSH, the firewall and systemd

There's no JavaScript in the web UI and no external services. All data lives on your server.

---

## Contents

1. [How it works](#how-it-works)
2. [Try it locally](#try-it-locally)
3. [Requirements](#requirements)
4. [Installing on a VPS](#installing-on-a-vps)
5. [Updating](#updating)
6. [Using it](#using-it)
7. [Administration](#administration)
8. [Backups and restore](#backups-and-restore)
9. [Access rules](#access-rules)
10. [Security](#security)
11. [Configuration reference](#configuration-reference)
12. [Files on the server](#files-on-the-server)
13. [Troubleshooting](#troubleshooting)
14. [Uninstalling](#uninstalling)
15. [Development](#development)
16. [Limitations](#limitations)
17. [Credits](#credits)

---

## How it works

```
browser        ─▶ Caddy :443 (HTTPS) ─▶ Anubis 127.0.0.1:8923 ─▶ gitserver 127.0.0.1:8080   web UI
git clone      ─▶ Caddy :443 (HTTPS) ───────────────────────────▶ gitserver                  public repos, read-only
git push/pull  ─▶ OpenSSH :22, user "git" ─▶ gitserver ssh-serve ─▶ git                       SSH key login
```

- **Caddy** terminates HTTPS. It gets a Let's Encrypt certificate automatically and renews it.
- **[Anubis](https://github.com/TecharoHQ/anubis)** sits in front of the web UI. It gives browsers a one-time proof-of-work check, so AI scrapers and crawlers can't hammer every commit and diff page. Git clients can't solve that check, so Caddy sends git's clone requests directly to gitserver.
- **gitserver** serves the web UI and HTTPS clones. It listens only on `127.0.0.1`, so it's never directly reachable from the internet.
- **Git over SSH** uses the server's normal OpenSSH with a dedicated `git` user. When you connect, sshd asks `gitserver ssh-keys` whether your key belongs to an account. If it does, the only thing that key can run is `gitserver ssh-serve`, which checks permissions and starts git. There's no shell, no port forwarding and no terminal. Your own admin SSH login is not affected.
- **Data:** repositories are plain bare git repositories on disk. Users, SSH keys and invites are in a SQLite database. The 2FA secrets in it are encrypted with a key kept outside the database.

---

## Try it locally

You need Go 1.26+ and git.

```sh
make build        # or: go build -o gitserver ./cmd/gitserver
./gitserver demo
```

`demo` starts a throwaway server on <http://127.0.0.1:8080> with:

- an admin account: username `demo`, password `demo-password`, and the current 2FA code, which is printed in the terminal every 30 seconds
- a public repo `~demo/hello` and a private repo `~demo/secret-notes`
- an invite link, to try the signup flow (you'll need any SSH public key)
- simulated git over SSH, so you can clone and push without an SSH server. The demo prints a `GIT_SSH_COMMAND=...` line:

  ```sh
  export GIT_SSH_COMMAND=/tmp/gitserver-demo-.../ssh
  git clone git@demo:~demo/hello
  ```

Everything is deleted when you press Ctrl-C. Use `-listen 127.0.0.1:9000` if port 8080 is taken. The demo only listens on loopback addresses, because its admin password is printed above.

To run with data that is kept, use your own authenticator app:

```sh
./gitserver user add -admin -data ./data yourname   # asks for SSH public key, password, shows a QR code
./gitserver serve -data ./data -insecure            # -insecure: allow login over plain http (local only!)
```

The 2FA encryption key is created at `./data/secret.key` the first time.

---

## Requirements

**Server**
- Debian 12+, Ubuntu 24.04+, or Fedora/RHEL with systemd. On Ubuntu 22.04 and RHEL, Caddy must come from Caddy's own repository or EPEL first; the installer stops with a hint if it can't find it.
- amd64 or arm64.
- OpenSSH server (any current version) and git 2.31+. The installer installs git; every current distro qualifies.
- **RAM: 1 GB plus 1 GB swap minimum, 2 GB recommended.** Idle use is small: gitserver ~25 MB, Anubis ~30 MB, Caddy ~45 MB. Each login's password check briefly uses 64 MB, so a burst of logins can push gitserver to about 500 MB.
- **Disk:** ~3 GB for the OS plus your repositories.

**Network**
- A domain name, e.g. `git.example.com`, with an **A record** (and an AAAA record only if the server really has working IPv6) pointing at the server.
- Ports **22** (SSH: your login and git), **80** (Let's Encrypt and the redirect to HTTPS) and **443** (web UI and HTTPS clone) reachable from the internet. If your hosting provider has its own firewall (e.g. the DigitalOcean Cloud Firewall), open them there too.

**Each user needs**
- git and an SSH key (`ssh-keygen -t ed25519`)
- an authenticator app: Aegis, 2FAS, Google Authenticator, 1Password, …

---

## Installing on a VPS

### Option A: download a release (recommended)

Every release on GitHub has a ready-made bundle; nothing needs to be built. On the server, as root:

```sh
case "$(uname -m)" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; esac
cd /tmp
wget -N https://github.com/obk/gitserver/releases/latest/download/gitserver-linux-$ARCH.tar.gz \
        https://github.com/obk/gitserver/releases/latest/download/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS    # must say OK
rm -rf bundle && tar -xzf gitserver-linux-$ARCH.tar.gz
DOMAIN=git.example.com sh bundle/deploy/install.sh
```

Use `sudo` before `sh` if you're not root. For a specific version, replace `latest/download` with `download/0.0.1` (the version tag). Without `wget`, use `curl -fLO URL` for each file.

### Option B: from your computer with `make deploy`

```sh
ssh root@git.example.com      # once, to accept the server's host key; then exit
make deploy HOST=root@git.example.com DOMAIN=git.example.com
```

This builds the Linux binary, copies a bundle to the server, and runs the installer there. If your computer has never connected to the server before, `make deploy` stops and tells you to run the `ssh` command above first. Otherwise SSH would silently wait for you to confirm the host key. For an ARM server, add `ARCH=arm64`.

### Option C: build the bundle yourself

```sh
make bundle                                               # creates dist/gitserver-<version>-linux-amd64.tar.gz
scp dist/gitserver-*-linux-amd64.tar.gz root@server:/tmp/
ssh root@server
tar -xzf /tmp/gitserver-*-linux-amd64.tar.gz -C /tmp
sudo sh /tmp/bundle/deploy/install.sh
```

### What the installer asks

When run in a terminal without `DOMAIN` set, it asks for:

1. **Domain name**, e.g. `git.example.com`. It warns if DNS doesn't point at this server yet.
2. **Site name** shown in the page header (default: the domain).
3. **Email for Let's Encrypt** expiry notices (optional).
4. **Confirmation** of the summary.
5. **Firewall:** if `ufw` is installed but off (as on a fresh Ubuntu droplet), whether to turn it on, allowing only SSH, 80 and 443. If ufw or firewalld is already on, whether to open 80/443 if they're blocked.
6. At the end: **create the first admin account now?** You'll paste the SSH *public* key from your own computer (`cat ~/.ssh/id_ed25519.pub`), choose a password, and scan a QR code with your authenticator app.

With `DOMAIN=…` set (as `make deploy` does), it skips the domain questions. See [installer variables](#installer-environment-variables) for fully unattended installs.

### What the installer does

1. Installs `git`, `caddy`, `curl` and `openssl` from your distribution, and **Anubis 1.27.0** from its GitHub release, checking the package's SHA-256.
2. Creates the system user **`git`** (home `/var/lib/gitserver`, no password, no usable shell).
3. Creates the **2FA encryption key** `/etc/gitserver/secret.key` (root only). It's never replaced on later runs.
4. Installs `/usr/local/bin/gitserver`, the admin helper `/usr/local/sbin/gitserverctl`, and the sandboxed systemd service.
5. Configures **Anubis**: a persistent signing key, metrics on localhost only, and a bot policy.
6. Configures **Caddy** for your domain, and adds an `import` line to `/etc/caddy/Caddyfile`, backing up an existing one first.
7. Adds **`/etc/ssh/sshd_config.d/50-gitserver.conf`**, a `Match User git` block. It checks the result with `sshd -t` and **rolls back automatically** if sshd rejects it, so it can't break your SSH access.
8. **Firewall** (see above). Before enabling ufw, it detects every port your SSH server listens on (including custom ports and Ubuntu's `ssh.socket`) and allows those first, so you can't lock yourself out.
9. Enables and starts everything, then checks that all three services run and that gitserver answers.
10. Waits up to 2 minutes for the **HTTPS certificate** and reports whether it was issued.

It ends with a summary: URLs, certificate status, SSH host key fingerprints, and next steps.

Everything runs **in the background** as systemd services and starts on boot. You can log out right after installing.

> **Save the 2FA key.** Copy `/etc/gitserver/secret.key` into your password manager (`sudo cat /etc/gitserver/secret.key`). Without it nobody can log in, and backups of the database can't be decrypted. See [Backups](#backups-and-restore).

### After installing

```sh
ssh git@git.example.com          # from your computer
# Hi ~yourname! You've successfully authenticated, but there is no shell access.
```

Then open `https://git.example.com`, log in, create a repository, and push.

---

## Updating

Download the newest release and run the installer again, the same commands as in [Option A](#option-a-download-a-release-recommended) without `DOMAIN=…`:

```sh
case "$(uname -m)" in x86_64) ARCH=amd64 ;; aarch64) ARCH=arm64 ;; esac
cd /tmp
wget -N https://github.com/obk/gitserver/releases/latest/download/gitserver-linux-$ARCH.tar.gz \
        https://github.com/obk/gitserver/releases/latest/download/SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
rm -rf bundle && tar -xzf gitserver-linux-$ARCH.tar.gz
sh bundle/deploy/install.sh
```

Or from your computer: `make update HOST=root@git.example.com`.

Re-running `install.sh` (or `make update`) runs as a **quick update** when gitserver is already installed:

- **no questions:** domain, site name and email are read from the existing setup
- **no package installs**, unless something is missing
- **only files whose content changed are replaced**, and **only affected services restart**: gitserver for a new binary or unit, Anubis for a new policy; Caddy is *reloaded* without downtime. If nothing changed, nothing restarts.
- a **progress bar** for each step, and a one-line summary:

```
[██████████░░░░░░░░] 4/7  Caddy (HTTPS)
      · site config unchanged
...
Updated git.example.com: gitserver binary.
  Version:  a1b2c3d -> e4f5a6b
```

Your data, the 2FA key and the Anubis signing key are never touched. To change settings (domain, site name, email, firewall), run with `RECONFIGURE=1`:

```sh
make deploy HOST=root@git.example.com DOMAIN=git.example.com RECONFIGURE=1
```

Package-manager output goes to `/var/log/gitserver-install.log`; on errors, the installer shows its last lines.

---

## Using it

### Logging in

Go to `https://git.example.com/login` and enter username, password and the 6-digit code from your authenticator app. Each code works only once.

The first visit to any page may briefly show Anubis' "Making sure you're not a bot!" page. It needs JavaScript once, then sets a cookie valid for 7 days. The gitserver pages themselves use no JavaScript.

### SSH keys

**Settings → SSH keys.** Paste the contents of `~/.ssh/id_ed25519.pub`:

- **Accepted types:** ed25519 (recommended), ECDSA, hardware security keys (`sk-ssh-ed25519@openssh.com`, `sk-ecdsa-…`), and RSA of at least 2048 bits. DSA is rejected.
- **One key, one account:** a key can belong to only one account.
- **At least one key:** you can't delete your last key, and an account without keys is sent to this page after login.
- **Limit:** up to 20 keys per account.
- **Instant effect:** deleting a key revokes its SSH access immediately.

### Repositories

- **Create:** **New repository** → name, optional description, private (default) or public. Names may use letters, digits, `.`, `_` and `-`, up to 100 characters, and must not end in `.git`.
- **Settings tab** (owner only): change the description or visibility, or **delete** the repository (type its name to confirm; this can't be undone).
- **Your repos** are listed at `https://git.example.com/~you`. The front page shows your repos plus everyone's public ones.
- **Browsing:** the web UI shows a summary (latest commits, README rendered from Markdown, clone URLs), file tree, files with line numbers and syntax highlighting, a raw file download, the commit log (50 per page), commits with highlighted diffs, branches and tags.

### Cloning and pushing

| | URL | Works for |
|---|---|---|
| HTTPS (read-only) | `https://git.example.com/~owner/repo` | public repos, anyone, no account needed |
| SSH | `git@git.example.com:~owner/repo` | everything your account may access; **the only way to push** |
| SSH, URL form | `ssh://git@git.example.com/~owner/repo` | same as above |

```sh
# new project
git remote add origin git@git.example.com:~you/project
git push -u origin main

# cloned over HTTPS, but you want to push: push over SSH
git remote set-url --push origin git@git.example.com:~you/project
```

Pushing over HTTPS is refused with a message that shows the SSH URL. Note the **`git@`** and the **`:`** in SSH URLs: without `git@`, SSH logs in as your local username and fails.

### Inviting people

Admins see **Settings → invites**. Create a link valid for 1, 7 or 30 days, optionally making the new user an admin. Send it privately; anyone with the link can use it once. The invited person:

1. opens the link, then picks a username, a password (12+ characters) and pastes their SSH public key
2. scans the 2FA QR code and enters one code
3. is logged in. The account only exists once that code checks out.

Unused invites can be revoked. Usernames are lowercase letters, digits, `-` and `_`, up to 32 characters. About 860 reserved names (`admin`, `root`, `support`, `api`, …) are blocked, so nobody can pose as staff.

### Password

**Settings → password.** It needs your current password and a 2FA code, and logs out all your other sessions.

### Landing page text

Logged-out visitors see a welcome page with your public repositories. Put your own text in `/var/lib/gitserver/intro.md` (Markdown; raw HTML is ignored). Changes show up immediately.

---

## Administration

On the server, use **`gitserverctl`**. It runs gitserver as the `git` user with the right data folder and the 2FA key, and re-runs itself with `sudo` if needed.

```sh
sudo gitserverctl status     # are gitserver, Anubis and Caddy running?
sudo gitserverctl logs       # follow all logs (Ctrl-C to stop)
sudo gitserverctl restart    # restart all three
sudo gitserverctl help       # list all commands
```

### Users

```sh
sudo gitserverctl user list                           # name, role, number of SSH keys
sudo gitserverctl user add -admin NAME                # create an account (SSH key, password, QR code)
sudo gitserverctl user passwd NAME                    # set a new password (forgotten password)
sudo gitserverctl user totp NAME                      # new authenticator (lost phone); shows a new QR code
sudo gitserverctl user admin NAME true|false          # grant/revoke admin (= may create invites)
sudo gitserverctl user key list NAME                  # SSH keys with IDs and fingerprints
sudo gitserverctl user key add NAME 'ssh-ed25519 AAAA… comment'
sudo gitserverctl user key del NAME KEY-ID
sudo gitserverctl user del NAME                       # delete the account (repos stay on disk)
```

Changing a password or 2FA ends all of that user's sessions immediately. Deleting a user revokes their SSH access at once. Their repositories stay in `/var/lib/gitserver/repos/NAME/` until you remove them. A user name can be taken only once: after deletion it stays reserved for good, so nobody can take over the old account's repositories or invites. To give the repositories to someone else, move the folder to their name.

### Invites

```sh
sudo gitserverctl invite create -base-url https://git.example.com            # 7 days
sudo gitserverctl invite create -admin -expires 24h -base-url https://git.example.com
sudo gitserverctl invite list
sudo gitserverctl invite revoke ID
```

### Repositories

```sh
sudo gitserverctl repo list
sudo gitserverctl repo create -public -desc "My project" '~owner/name'
sudo gitserverctl repo public '~owner/name'
sudo gitserverctl repo private '~owner/name'
```

Existing bare repositories can be copied to `/var/lib/gitserver/repos/OWNER/NAME.git` (owned by `git:git`). Create the user first, or use `user add`, which warns about the existing folder; web signup refuses such names. Names that ever had an account can't be used again. A repo is public exactly when the file `git-daemon-export-ok` exists inside it, and its description is the file `description`.

### Logs

- Web requests and logins: `journalctl -u gitserver`. Each line has the client IP; failed logins are logged with the IP.
- Git over SSH, an audit log of every clone, push and rejected command: `journalctl -t gitserver-ssh`.
- Anubis and Caddy: `journalctl -u anubis@gitserver`, `journalctl -u caddy`, and `/var/log/caddy/gitserver.log`.

---

## Backups and restore

Back up three things:

| What | Where | How |
|---|---|---|
| Repositories | `/var/lib/gitserver/repos/` | copy the directory (rsync, restic, borg, …) |
| Database (users, keys, invites) | `/var/lib/gitserver/gitserver.db` | `sudo gitserverctl backup /var/lib/gitserver/backup-$(date +%F).db`, then copy that file |
| **2FA key** | `/etc/gitserver/secret.key` | once, into your password manager, **separately** from the backups |

`gitserverctl backup` writes a consistent snapshot while the server runs. Don't just copy `gitserver.db`: recent changes may still be in `gitserver.db-wal`. Backup files are created with mode `0600`.

Keeping the key apart from the backups is deliberate: a stolen backup can't reveal anyone's 2FA secret. If you lose the key, the data is still fine, but every user has to re-enroll 2FA (`gitserverctl user totp NAME`).

**Restore** on a fresh install of the same version:

```sh
sudo systemctl stop gitserver
sudo cp backup.db /var/lib/gitserver/gitserver.db
sudo rm -f /var/lib/gitserver/gitserver.db-wal /var/lib/gitserver/gitserver.db-shm
sudo rsync -a repos/ /var/lib/gitserver/repos/
sudo cp secret.key /etc/gitserver/secret.key && sudo chmod 600 /etc/gitserver/secret.key
sudo chown -R git:git /var/lib/gitserver
sudo systemctl start gitserver
```

If the key doesn't match the database, gitserver refuses to start and says so.

---

## Access rules

| | Anonymous | Logged-in user | Owner |
|---|---|---|---|
| Public repo: browse in the web UI | ✔ | ✔ | ✔ |
| Public repo: clone over HTTPS | ✔ | ✔ | ✔ |
| Public repo: clone over SSH | needs an account | ✔ | ✔ |
| Private repo: see, browse, clone | ✘ (404) | ✘ (404) | ✔ |
| Push, settings, delete | ✘ | ✘ | ✔ |
| Create invites | ✘ | admins only | (admins only) |

**Admins have no extra access to repositories.** They can only create invites, and the server operator manages everything else from the command line.

---

## Security

Every claim below links to the code that implements it. On this server and on GitHub, the links open the file at that line. `make test` checks that each link still points at the named code ([`TestReadmeCodeLinks`](readme_test.go#L16)).

### Accounts

- **Passwords** are hashed with **argon2id** (64 MiB, 3 passes, 4 lanes, 16-byte random salt), the algorithm OWASP and RFC 9106 recommend. The plaintext is never stored or logged. Unknown usernames are checked against a dummy hash, so response timing doesn't reveal which accounts exist. At most 4 hashes run at once, and a request waits at most 5 s for a slot before it gets a "server busy" page, so a login flood can't queue up without limit. Code: [`argonWait`](internal/account/password.go#L34), [`argonTime`](internal/account/password.go#L21), [`HashPassword`](internal/account/password.go#L47), [`CheckPassword`](internal/account/password.go#L69), [`DummyHash`](internal/account/password.go#L104), [`argonSem`](internal/account/password.go#L32).
- **Two-factor authentication** (TOTP, RFC 6238) is required for everyone. Codes allow ±30 s of clock skew, and each code works only once. The code is only checked after the password is correct. Code: [`CheckTOTP`](internal/account/totp.go#L46), [`totpSkew`](internal/account/totp.go#L20), [`authenticate`](internal/web/login.go#L105).
- **2FA secrets are encrypted** with AES-256-GCM. The key lives in `/etc/gitserver/secret.key` (root only), never in the database: Code: [`SealTOTP`](internal/account/secretbox.go#L56), [`OpenTOTP`](internal/account/secretbox.go#L65).
  - the service receives it through systemd `LoadCredential`, and the `git` user can't read the file Code: [`LoadCredential`](deploy/gitserver.service#L13), [`secret.key`](deploy/install.sh#L396), [`run`](deploy/gitserverctl#L16).
  - each secret is bound to its username, so it can't be moved into another account Code: [`totpAD`](internal/account/secretbox.go#L54).
  - a stolen database or backup holds only ciphertext Code: [`Backup`](internal/store/store.go#L166).
  - gitserver refuses to start with a missing or wrong key, instead of silently breaking logins Code: [`LoadSecretBox`](internal/store/secretkey.go#L40).
  - old plaintext secrets are encrypted automatically and wiped from the database file Code: [`EncryptTOTPSecrets`](internal/store/secretkey.go#L120), [`purgeFreedPages`](internal/store/store.go#L156).
- **Brute force:** after 10 failed attempts in 15 minutes, a client gets HTTP 429. Wrong passwords count **per IP** (per /64 for IPv6). Only wrong 2FA codes **after a correct password** count per account, so a stranger can't lock you out by guessing. Each attempt counts against the IP before the password is checked, so parallel requests can't get past the limit. Code: [`take`](internal/web/ratelimit.go#L48), [`newLimiter`](internal/web/ratelimit.go#L21), [`ipKey`](internal/web/middleware.go#L76), [`passwordOK`](internal/web/login.go#L65).
- **Warning about a known password:** wrong 2FA codes entered with the correct password mean someone may know it. Instead of locking the account (which would let that person lock you out), your next login opens the password page and says how many wrong codes were tried since when. Code: [`codeAlerts`](internal/web/ratelimit.go#L103), [`alerts.take`](internal/web/login.go#L88).
- **Sessions:** server-side, with 256-bit random IDs: Code: [`create`](internal/web/session.go#L51).
  - the cookie is `__Host-` prefixed, `Secure`, `HttpOnly` and `SameSite=Strict` Code: [`startSession`](internal/web/session.go#L195).
  - sessions last at most 12 h, or 2 h idle, and get a fresh ID at every login Code: [`sessionMaxAge`](internal/web/session.go#L17).
  - they end immediately when the password or 2FA secret changes, even if changed from the command line Code: [`credentialFingerprint`](internal/web/session.go#L47), [`withSession`](internal/web/session.go#L120).
- **CSRF:** every form has a per-session token, plus Go's `http.CrossOriginProtection`. Redirects after login only go to local paths. Code: [`validCSRF`](internal/web/session.go#L210), [`NewCrossOriginProtection`](internal/web/server.go#L170), [`safeNext`](internal/web/login.go#L17).
- **User names are used once:** every name that ever had an account is recorded, and a deleted account's name can never be taken again, so a newcomer can't inherit its repositories. A database trigger records each new name; on upgrade, existing users, invite records and repository folders are recorded too. Code: [`usedNamesSchema`](internal/store/store.go#L181), [`ErrNameUsed`](internal/store/store.go#L65).
- **Invites** are random 192-bit codes, single-use with expiry, and only their SHA-256 hash is stored. Admin rights come from the invite, never from the signup form. Code: [`CreateInvite`](internal/store/store.go#L559), [`RedeemInvite`](internal/store/store.go#L632).

### Git

- **SSH:** keys are looked up live in the database by sshd's `AuthorizedKeysCommand`. Every key is restricted (`restrict,command="gitserver ssh-serve USER"`): no shell, PTY, port forwarding or user rc files. Code: [`Match User git`](deploy/sshd-gitserver.conf#L6), [`AuthorizedKeys`](internal/sshgit/ssh.go#L101), [`authorizedKeyLine`](internal/sshgit/ssh.go#L131).
  - `ssh-serve` accepts only `git-upload-pack`, `git-receive-pack` and `git-upload-archive` with a strictly validated `'~owner/repo'` path, checks permissions, and runs git directly without a shell. Code: [`parseSSHCommand`](internal/sshgit/ssh.go#L202), [`sshRepoArgRe`](internal/sshgit/ssh.go#L199), [`Serve`](internal/sshgit/ssh.go#L217), [`syscall.Exec`](internal/sshgit/ssh.go#L286).
  - The sshd config applies only to the `git` user. Code: [`Match User git`](deploy/sshd-gitserver.conf#L6).
  - All of this was tested against a real OpenSSH server: clone, push, shell attempts, port forwarding and unknown keys. The automated test runs the same key lookup and forced command. Code: [`TestGitSSH`](internal/web/server_test.go#L211).
- **HTTPS clone:** only git's smart-HTTP `git-upload-pack`, only public repositories, with pushing disabled. There's no dumb protocol and no direct file access. Clones have their own concurrency limit and end after 30 minutes, so stalled clients can't hold the slots, and credentials or cookies are stripped before git runs. Code: [`serveGitHTTP`](internal/web/gitclone.go#L30), [`cloneTimeout`](internal/web/gitclone.go#L28), [`gitHTTPRe`](internal/web/gitclone.go#L17), [`cloneSlots`](internal/web/gitclone.go#L20), [`Authorization`](internal/web/gitclone.go#L80).
- **Removed commits stay removed:** after a force push or a deleted branch, the old commits stay in the repository until `git gc` prunes them. The web UI only shows commits that a branch or tag reaches, and commit pages need the full hash (a short one redirects to it, if the commit is reachable). Git protocol v2 is not offered over SSH or HTTPS because its upload-pack serves any object by hash; clients fall back to v0/v1, which only serve what the refs reach. To delete a leaked secret from the disk too, run `git gc --prune=now` in the repository. Code: [`reachable`](internal/gitrepo/git.go#L146), [`Git-Protocol`](internal/web/gitclone.go#L82), [`version=1`](internal/sshgit/ssh.go#L275).
- **No information leaks:** private and missing repositories give the same answer on the web (404), over HTTPS ("Repository not found") and over SSH ("not found or access denied"). Code: [`handleRepo`](internal/web/repos.go#L144), [`Repository not found`](internal/web/gitclone.go#L53), [`not found or access denied`](internal/sshgit/ssh.go#L251).
- **Hardening:**
  - repository names, refs and paths are validated, and git never runs through a shell Code: [`validRepoName`](internal/gitrepo/repo.go#L39), [`validRev`](internal/gitrepo/git.go#L104), [`cleanTreePath`](internal/web/repos.go#L121), [`Command`](internal/gitrepo/git.go#L56).
  - diffs use `--no-ext-diff --no-textconv`, so repository content can't make git run programs Code: [`--no-textconv`](internal/gitrepo/git.go#L179).
  - web git processes are capped and time out after 30 s Code: [`gitSlots`](internal/gitrepo/git.go#L41), [`Timeout`](internal/gitrepo/git.go#L22).
- **Limits per account and client,** so one user or address can't fill the disk or take all the capacity:
  - a push may send at most 1 GiB (git's `receive.maxInputSize`), and pushes are refused while less than 1 GiB of disk is free Code: [`maxPushSize`](internal/sshgit/ssh.go#L152), [`minFreeDisk`](internal/sshgit/ssh.go#L153).
  - each user runs at most 4 git operations over SSH at once. The count is kept in lock files that git holds until it exits, so crashed processes free their slot Code: [`MaxGitPerUser`](internal/sshgit/ssh.go#L154), [`AcquireUserSlot`](internal/sshgit/ssh.go#L165).
  - each client address (IPv6: each /64) runs at most 2 HTTPS clones at once Code: [`clonesPerIP`](internal/web/gitclone.go#L24).
  - each user can create at most 100 repositories in the web UI; the command line isn't limited Code: [`maxReposPerUser`](internal/web/repos.go#L35).
  - to change a limit, edit the constant and rebuild.

### Web

- **Content Security Policy** with **no scripts at all**: `default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'`. The 2FA QR code is inline SVG; syntax highlighting uses CSS classes. Code: [`secureHeaders`](internal/web/middleware.go#L11), [`qrSVG`](internal/web/signup.go#L259), [`tokenClass`](internal/render/highlight.go#L93).
- **Other headers:** HSTS, `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: same-origin`, cross-origin isolation headers, and `Cache-Control: no-store` on pages. Code: [`secureHeaders`](internal/web/middleware.go#L11), [`no-store`](internal/web/page.go#L81).
- **Raw files** are served as `text/plain` with `Content-Security-Policy: sandbox`, so a repository can't host active content on your domain. Images (png, jpg, gif, webp, svg) keep their type so READMEs can show them, still sandboxed. An SVG opened directly (not as an image) is downloaded instead of shown. Code: [`handleRaw`](internal/web/repos.go#L392), [`sandbox`](internal/web/repos.go#L422), [`Content-Disposition`](internal/web/repos.go#L427), [`RawContentType`](internal/render/markdown.go#L85).
- **Markdown** (READMEs, intro) is rendered without raw HTML and without `javascript:` links. External images are blocked by the CSP. Relative links in a README open the file view, like on GitHub. Code: [`Markdown`](internal/render/markdown.go#L27), [`rewriteRelative`](internal/render/markdown.go#L59).
- **Bot protection:** Anubis in front of the web UI. Its robots.txt asks all crawlers to stay away (change `SERVE_ROBOTS_TXT` in `/etc/anubis/gitserver.env` if you want search engines). Code: [`SERVE_ROBOTS_TXT`](deploy/anubis.env#L14), [`generic-browser`](deploy/anubis.botPolicies.yaml#L22).

### Server

- **The systemd unit is sandboxed:** `ProtectSystem=strict`, no capabilities, a syscall filter, private /tmp and devices. `systemd-analyze security` rates it 1.3 ("OK"; lower is better). Code: [`Hardening`](deploy/gitserver.service#L22).
- **Nothing extra faces the internet:** gitserver, Anubis and its metrics listen only on `127.0.0.1`. Code: [`127.0.0.1:8080`](deploy/gitserver.service#L14), [`BIND`](deploy/anubis.env#L4), [`METRICS_BIND`](deploy/anubis.env#L10).
- **Private files:** the data folder is `0700`, and the database and backups are `0600`. SQLite `secure_delete` is on, so deleted data is overwritten. Code: [`0700`](deploy/install.sh#L391), [`0o600`](internal/store/store.go#L117), [`secure_delete`](internal/store/store.go#L124).

---

## Configuration reference

### `gitserver serve` flags

| Flag | Default | Meaning |
|---|---|---|
| `-data DIR` | `$GITSERVER_DATA` or `./data` | data folder (all commands accept it) |
| `-listen ADDR` | `127.0.0.1:8080` | listen address |
| `-base-url URL` | from request | public URL, e.g. `https://git.example.com`, used for clone and invite links |
| `-ssh-host HOST` | host of `-base-url` | host shown in SSH clone URLs |
| `-site NAME` | `git` | site name in the page header |
| `-tls-cert FILE -tls-key FILE` | – | serve HTTPS directly (without Caddy) |
| `-trust-proxy` | off | trust the reverse proxy's `X-Forwarded-*` headers |
| `-real-ip-header NAME` | – | with `-trust-proxy`: header holding the client IP (`X-Real-IP` behind Caddy) |
| `-insecure` | off | allow login cookies over plain HTTP, **for local testing only** |

### Environment variables

| Variable | Meaning |
|---|---|
| `GITSERVER_DATA` | default data folder |
| `GITSERVER_KEY` | 2FA encryption key as 64 hex characters (used by `gitserverctl`) |
| `GITSERVER_KEY_FILE` | path to the key file |
| `CREDENTIALS_DIRECTORY` | set by systemd; the key is read from `secret.key` in it |

The key is looked up in this order: `GITSERVER_KEY`, the systemd credential, `GITSERVER_KEY_FILE`, then `<data>/secret.key`, which is created automatically for local use.

### Installer environment variables

| Variable | Meaning |
|---|---|
| `DOMAIN` | domain name; skips the domain questions |
| `SITE_NAME` | header name (default: domain) |
| `ACME_EMAIL` | email for Let's Encrypt expiry notices |
| `ENABLE_UFW=1` | turn on ufw (SSH, 80, 443) without asking |
| `RECONFIGURE=1` | full install with questions, even if already installed |
| `CERT_WAIT_SECONDS` | how long to wait for the certificate (default 120) |
| `ANUBIS_VERSION`, `ANUBIS_SHA256` | install another Anubis version (needs its checksum) |
| `BIN` | path to the gitserver binary (default: next to `deploy/`) |

### Limits

| | |
|---|---|
| SSH keys per account | 20 |
| Session lifetime | 12 h (2 h idle) |
| Login attempts | 10 failures per 15 min (per IP; per account for wrong 2FA codes) |
| Invite validity | 1, 7 or 30 days (web); any duration (CLI) |
| Signup | 15 minutes between step 1 and the 2FA confirmation |
| File view | files up to 1 MiB are shown; larger ones via "View raw" |
| Syntax highlighting | files up to 512 KiB / 2 s; diffs up to 1 MiB |
| Diff view | up to 2 MiB, then truncated |
| Log | 50 commits per page |
| Push size | 1 GiB per push; pushes stop while less than 1 GiB of disk is free |
| Git over SSH | 4 operations at once per user |
| HTTPS clone | 2 at once per client address (IPv6: per /64) |
| Repositories | 100 per user from the web UI (the command line isn't limited) |

---

## Files on the server

| Path | What |
|---|---|
| `/usr/local/bin/gitserver` | the program |
| `/usr/local/sbin/gitserverctl` | admin helper |
| `/var/lib/gitserver/` | data (`git:git`, `0700`) |
| `/var/lib/gitserver/gitserver.db` | SQLite database (+ `-wal`, `-shm`) |
| `/var/lib/gitserver/repos/OWNER/NAME.git` | bare repositories |
| `/var/lib/gitserver/intro.md` | optional landing page text |
| `/etc/gitserver/secret.key` | 2FA encryption key (root, `0600`) |
| `/etc/systemd/system/gitserver.service` | systemd unit |
| `/etc/ssh/sshd_config.d/50-gitserver.conf` | sshd config for the `git` user |
| `/etc/anubis/gitserver.env`, `gitserver.botPolicies.yaml` | Anubis config and bot policy |
| `/etc/caddy/gitserver.caddy` (+ import in `/etc/caddy/Caddyfile`) | Caddy site |
| `/var/log/caddy/gitserver.log` | Caddy access log |
| `/var/log/gitserver-install.log` | installer log |

Source files in `deploy/` map to these: `gitserver.service`, `sshd-gitserver.conf`, `gitserver.caddy`, `anubis.env`, `anubis.botPolicies.yaml`, `gitserverctl`, `install.sh`.

---

## Troubleshooting

**`make deploy` stops with "has not connected to … before".** Run `ssh root@server` once, check the fingerprint, answer `yes`, and try again.

**`git push`: `Permission denied (publickey)` and the message shows `you@server`, not `git@server`.** The remote URL is missing `git@`. Fix it:
`git remote set-url origin git@git.example.com:~owner/repo`.

**`ssh git@server`: `Permission denied (publickey)`.**
- Is the key added under Settings → SSH keys? Is your SSH client offering it? Check with `ssh -v git@server`.
- On Fedora/RHEL with SELinux, check `ausearch -m avc -ts recent`.

**`Repository not found` when cloning.** Over HTTPS, only public repositories can be cloned; clone private ones over SSH. Over SSH, check the owner and name (`~owner/repo`) and that you're the owner or the repo is public.

**HTTPS certificate "NOT issued".**
- The domain's A record must point at the server.
- An AAAA (IPv6) record must only exist if the server really answers on that IPv6 address. Let's Encrypt prefers IPv6.
- Ports 80 and 443 must be open in ufw *and* in your provider's firewall.
- Check with `journalctl -u caddy | grep -iE 'acme|challenge|certificate'`. Caddy keeps retrying on its own.

**2FA code not accepted.**
- Check the phone's clock; codes allow only ±30 s.
- Each code works once, so wait for the next one.
- Lost phone: `sudo gitserverctl user totp NAME`.

**"Too many failed attempts".** Wait 15 minutes. The limit is per IP for wrong passwords.

**gitserver doesn't start: "this key does not match the database" / "key … is missing".** Restore `/etc/gitserver/secret.key` from your password manager, then `sudo systemctl restart gitserver`. If the key is truly lost, temporarily move the database aside to start fresh, or re-enroll every user's 2FA.

**The page says "Making sure you're not a bot!" and doesn't continue.** That's Anubis. Enable JavaScript for the site once; the cookie lasts 7 days.

**Anything else:** `sudo gitserverctl status` and `sudo gitserverctl logs`.

---

## Uninstalling

```sh
sudo systemctl disable --now gitserver anubis@gitserver
sudo rm /etc/ssh/sshd_config.d/50-gitserver.conf && sudo systemctl try-reload-or-restart ssh   # 'sshd' on Fedora
sudo rm /etc/caddy/gitserver.caddy
sudo sed -i '/import \/etc\/caddy\/gitserver.caddy/d' /etc/caddy/Caddyfile && sudo systemctl reload caddy
sudo rm /etc/systemd/system/gitserver.service /usr/local/bin/gitserver /usr/local/sbin/gitserverctl
sudo systemctl daemon-reload
# Data and keys; back them up first if you want to keep them:
#   /var/lib/gitserver  /etc/gitserver  /etc/anubis/gitserver.*
```

---

## Development

```sh
make build        # ./gitserver
make test         # go vet + all tests (real git; includes the SSH forced-command flow)
make dist         # linux amd64 + arm64 binaries in dist/
make bundle       # deploy bundle for ARCH (default amd64)
./gitserver demo
```

**Project layout.** The program is one binary, built from `cmd/gitserver`. Everything else is in `internal/`, split by job; each package only imports the ones listed above it:

| Folder | What |
|---|---|
| `cmd/gitserver/` | the command line (`serve`, `user`, `invite`, `repo`, `backup`, `ssh-keys`, `ssh-serve`) and `demo` |
| `internal/account/` | rules that need no storage: user names and the reserved-name `blocklists/`, password hashing, TOTP codes, random tokens, encryption of 2FA secrets |
| `internal/render/` | Markdown and syntax highlighting, turned into safe HTML |
| `internal/store/` | the SQLite database (users, SSH keys, invites, used names) and loading the 2FA encryption key |
| `internal/gitrepo/` | repositories on disk and the git commands that read them |
| `internal/sshgit/` | git over SSH: key parsing, sshd's key lookup, the forced command and its limits |
| `internal/web/` | the web UI: routes (`server.go`), pages by feature (`home.go`, `login.go`, `signup.go`, `settings.go`, `invites.go`, `repos.go`), HTTPS clone (`gitclone.go`), sessions, rate limits, and the embedded `templates/` and `static/` CSS |
| `deploy/` | installer and server configs |
| `test/smoke/` | container that acts as a fresh VPS, for testing the installer |
| `readme_test.go` | checks the code links in this README |

Tests sit next to the code they test. The tests in `internal/web` run a full server with real git, including the SSH key lookup and forced command (they build the binary first).

The input checks that face attackers (SSH command, revisions, tree paths, login redirects, README links) have fuzz tests in each package's `fuzz_test.go`. `make test` runs their seed inputs; to search for new failures, run one for a while, e.g. `go test -run '^$' -fuzz '^FuzzSafeNext$' -fuzztime 5m ./internal/web`. A failing input is saved under that package's `testdata/fuzz/` and then runs with every `make test`.

**Releases.** Push a version tag and GitHub Actions (`.github/workflows/release.yml`) runs the tests, builds the amd64 and arm64 bundles and publishes them as a GitHub Release with `SHA256SUMS`:

```sh
git tag 0.0.2 && git push origin 0.0.2
```

Or on GitHub: **Actions → release → Run workflow**, enter the version (e.g. `0.0.2`); this tags the latest commit of the branch you pick (normally `main`).

The bundle names don't contain the version, so the `latest/download` links in [Option A](#option-a-download-a-release-recommended) always get the newest release.

GitHub Actions (`.github/workflows/ci.yml`) runs gofmt, `make test` and [govulncheck](https://go.dev/doc/security/vuln/) on every push and pull request. Once a week it also fuzzes every target for 5 minutes and checks for newly published vulnerabilities.

To smoke-test the installer locally without a VPS:

```sh
podman build -t gitserver-smoke test/smoke
podman run -d --name gs --privileged --systemd=always -p 127.0.0.1:2223:22 -p 127.0.0.1:8444:443 gitserver-smoke
make bundle && podman cp dist/gitserver-*-linux-amd64.tar.gz gs:/tmp/b.tgz
podman exec gs sh -c 'mkdir /tmp/b && tar -C /tmp/b --strip-components=1 -xzf /tmp/b.tgz'
podman exec -e DOMAIN=localhost gs sh /tmp/b/deploy/install.sh
```

`DOMAIN=localhost` makes Caddy use a local certificate instead of Let's Encrypt.

---

## Limitations

- **No collaborators:** only a repository's owner can push.
- **No HTTPS cloning of private repositories:** that would need HTTPS credentials. Private repos are SSH-only.
- **Restarts log everyone out:** sessions are kept in memory.
- **Not in the web UI:** issues, pull requests, CI and webhooks. This is a git host, not a forge.
- **Manual recovery:** a lost password or phone needs the server admin (`gitserverctl user passwd/totp`).

## Credits

- Look inspired by [sourcehut](https://sourcehut.org) and [stagit](https://codemadness.org/stagit.html)
- [Anubis](https://github.com/TecharoHQ/anubis) by Techaro (bot protection), [Caddy](https://caddyserver.com) (HTTPS)
- Go libraries:
  - [chroma](https://github.com/alecthomas/chroma) for syntax highlighting
  - [goldmark](https://github.com/yuin/goldmark) for Markdown
  - [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) for the database
  - [rsc.io/qr](https://pkg.go.dev/rsc.io/qr) for QR codes
  - [golang.org/x/crypto](https://pkg.go.dev/golang.org/x/crypto) for argon2 and SSH
- Username blocklists from [shouldbee/reserved-usernames](https://github.com/shouldbee/reserved-usernames) and [marteinn/The-Big-Username-Blocklist](https://github.com/marteinn/The-Big-Username-Blocklist) (MIT, see `internal/account/blocklists/`)
