# gitserver

A small, self-hosted git server written in Go, styled after sourcehut's [git.sr.ht](https://git.sr.ht).

It's a single program that gives you:

- a **web UI** to browse repositories: summary with README, file tree, log, refs, commits with diffs, and syntax highlighting
- **git over SSH** for pushing and private repos: `git@git.example.com:~you/project`
- **read-only HTTPS clone** for public repos: `https://git.example.com/~you/project`
- **accounts with mandatory two-factor login** (password + authenticator app), invite-only signup, and SSH keys
- a ready-made **VPS installer** that sets up Caddy (automatic HTTPS), Anubis (bot protection), OpenSSH, the firewall, automatic security updates, fail2ban and systemd

There's no JavaScript in the web UI and no external services. All data lives on your server.

**Documentation:** installing, updating, using and running it are all in [WIKI.md](WIKI.md), also published as the [GitHub wiki](https://github.com/obk/gitserver/wiki).

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
