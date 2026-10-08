#!/bin/sh
# Install or update gitserver + Anubis + Caddy + git over OpenSSH on a systemd
# VPS (Debian/Ubuntu or Fedora/RHEL family). Run as root from an unpacked bundle:
#
#   bundle/gitserver          the linux binary
#   bundle/deploy/...         this directory
#
#   sudo sh bundle/deploy/install.sh                       # asks for the domain
#   sudo DOMAIN=git.example.com sh bundle/deploy/install.sh # no questions
#
# If gitserver is already installed (and DOMAIN is unset or the same), it
# runs as a quick update: no questions, no package installs, and only files
# that changed are replaced and only affected services restarted.
#
# Optional: ACME_EMAIL=you@example.com for Let's Encrypt expiry notices,
#           ENABLE_UFW=1 to turn on ufw (SSH, 80, 443 only) without asking,
#           AUTO_UPDATES=0 / FAIL2BAN=0 to skip automatic security updates /
#           fail2ban for SSH (Debian/Ubuntu; both are on by default),
#           RECONFIGURE=1 to run the full install (with questions) again.
#
# It never touches the data in /var/lib/gitserver, the 2FA key in
# /etc/gitserver/secret.key, or the Anubis signing key.
set -eu

ANUBIS_VERSION=${ANUBIS_VERSION:-1.27.0}
LOGFILE=/var/log/gitserver-install.log

HERE=$(cd "$(dirname "$0")" && pwd)
BIN=${BIN:-$HERE/../gitserver}

# ---------------------------------------------------------------- output

TTY=0
[ -t 1 ] && TTY=1
bold() { if [ "$TTY" = 1 ]; then printf '\033[1m%s\033[0m\n' "$*"; else printf '%s\n' "$*"; fi; }
info() { printf '      %s\n' "$*"; }
ok() { if [ "$TTY" = 1 ]; then printf '      \033[32m✓\033[0m %s\n' "$*"; else printf '      ok: %s\n' "$*"; fi; }
same() { if [ "$TTY" = 1 ]; then printf '      \033[2m· %s\033[0m\n' "$*"; else printf '      unchanged: %s\n' "$*"; fi; }
die() { printf '\nerror: %s\n' "$*" >&2; exit 1; }
warn() { if [ "$TTY" = 1 ]; then printf '      \033[33m! %s\033[0m\n' "$*" >&2; else printf '      warning: %s\n' "$*" >&2; fi; }

STEP=0
TOTAL=1
# step "title": prints a progress bar line, e.g. [████████░░░░░░] 4/7 Caddy
step() {
	STEP=$((STEP + 1))
	if [ "$TTY" = 1 ]; then
		width=18 filled=$((STEP * 18 / TOTAL)) bar="" i=0
		while [ "$i" -lt "$width" ]; do
			if [ "$i" -lt "$filled" ]; then bar="$bar█"; else bar="$bar░"; fi
			i=$((i + 1))
		done
		printf '\033[1m[%s] %d/%d  %s\033[0m\n' "$bar" "$STEP" "$TOTAL" "$*"
	else
		printf '==> [%d/%d] %s\n' "$STEP" "$TOTAL" "$*"
	fi
}

# spin "what" SECONDS: on a terminal, shows that something is still going
# on: "⠹ what (12s)" on one line, redrawn in place. spin_end clears it.
spin() {
	[ "$TTY" = 1 ] || return 0
	SPIN_N=$((${SPIN_N:-0} + 1))
	case $((SPIN_N % 4)) in 0) f='⠋' ;; 1) f='⠙' ;; 2) f='⠸' ;; *) f='⠴' ;; esac
	printf '\r      \033[36m%s\033[0m %.60s \033[2m(%ss)\033[0m\033[K' "$f" "$1" "$2"
}
spin_end() { [ "$TTY" = 1 ] && printf '\r\033[K' || true; }

# quiet runs a noisy command with its output in $LOGFILE, with a spinner
# while it runs; on failure it shows the end of that output and stops.
quiet() {
	printf '\n### %s\n' "$*" >>"$LOGFILE"
	if [ "$TTY" = 1 ]; then
		# The exit status goes to a file: a finished child stays a zombie
		# until "wait", so kill -0 can't tell whether it is done.
		rc=$(mktemp)
		{
			"$@" >>"$LOGFILE" 2>&1 && echo 0 >"$rc" || echo 1 >"$rc"
		} </dev/null &
		n=0
		while [ ! -s "$rc" ]; do
			spin "$*" $((n / 5))
			sleep 0.2
			n=$((n + 1))
		done
		wait
		spin_end
		status=$(cat "$rc")
		rm -f "$rc"
		[ "$status" = 0 ] && return 0
	elif "$@" >>"$LOGFILE" 2>&1; then
		return 0
	fi
	tail -n 20 "$LOGFILE" >&2
	die "'$*' failed (full output: $LOGFILE)"
}

# ---------------------------------------------------------------- questions

# Questions are only asked when run from a terminal; otherwise everything
# comes from environment variables (DOMAIN, SITE_NAME, ...).
INTERACTIVE=0
if [ -t 0 ] && [ -t 1 ] && { : </dev/tty; } 2>/dev/null; then
	INTERACTIVE=1
fi
ask() { # ask "question" "default" -> prints the answer
	if [ -n "$2" ]; then
		printf '%s [%s]: ' "$1" "$2" >/dev/tty
	else
		printf '%s: ' "$1" >/dev/tty
	fi
	IFS= read -r answer </dev/tty || answer=""
	printf '%s' "${answer:-$2}"
}
yes_no() { # yes_no "question" -> true for yes (default yes)
	case "$(ask "$1 [Y/n]" "y")" in [Yy]*) return 0 ;; *) return 1 ;; esac
}

# ---------------------------------------------------------------- helpers

CHANGED=""
# put SRC DEST MODE: install SRC as DEST if the content differs. Returns
# success only if DEST was created or changed.
put() {
	if [ -f "$2" ] && cmp -s "$1" "$2"; then
		chmod "$3" "$2"
		return 1
	fi
	install -m "$3" "$1" "$2"
	return 0
}
changed() { CHANGED="$CHANGED, $1"; }

# deb_installed PKG: is the package installed? (dpkg -s also succeeds for a
# removed package whose config files are still there.)
deb_installed() {
	[ "$(dpkg-query -W -f '${Status}' "$1" 2>/dev/null)" = "install ok installed" ]
}

# set_env FILE KEY=VALUE: set KEY in an env file, replacing its line or
# adding one. Returns success only if FILE changed.
set_env() {
	grep -qxF "$2" "$1" && return 1
	key=${2%%=*}
	if grep -q "^$key=" "$1"; then
		sed -i "s|^$key=.*|$(printf '%s' "$2" | sed 's/[&|\\]/\\&/g')|" "$1"
	else
		printf '%s\n' "$2" >>"$1"
	fi
}

# Domains Caddy serves with its own local CA instead of Let's Encrypt.
internal_domain() {
	case "$1" in localhost | *.localhost | *.local | *.internal | *.home.arpa) return 0 ;; esac
	printf '%s' "$1" | grep -Eq '^[0-9.]+$'
}

# check_dns warns if DOMAIN does not point at this machine.
check_dns() {
	internal_domain "$DOMAIN" && return 0
	resolved=$(getent ahosts "$DOMAIN" 2>/dev/null | awk '{print $1}' | sort -u)
	if [ -z "$resolved" ]; then
		warn "$DOMAIN does not resolve yet. Add an A (and/or AAAA) record pointing to this server. Caddy keeps retrying until it does."
		return 1
	fi
	mine=$( (ip -o addr show scope global 2>/dev/null | awk '{split($4,a,"/"); print a[1]}'; hostname -I 2>/dev/null | tr ' ' '\n') | sort -u)
	for ip in $resolved; do
		printf '%s\n' "$mine" | grep -qx "$ip" && return 0
	done
	warn "$DOMAIN points to $(echo $resolved), but this server has $(echo $mine). Fine if that is this server's public/NAT address; otherwise Let's Encrypt will fail."
	return 1
}

# ssh_ports lists the TCP ports the SSH server listens on (sshd_config,
# running sshd, and Ubuntu's socket-activated ssh.socket), default 22.
ssh_ports() {
	ports=$(
		{
			"$SSHD" -T 2>/dev/null | awk '$1 == "port" {print $2}'
			ss -Htlnp 2>/dev/null | awk '/"sshd/ {n = split($4, a, ":"); print a[n]}'
			systemctl cat ssh.socket 2>/dev/null | sed -n 's/^ListenStream=\(.*[^0-9]\)\{0,1\}\([0-9][0-9]*\)$/\2/p'
		} | grep -E '^[0-9]+$' | sort -un | tr '\n' ' '
	)
	echo "${ports% }" | sed 's/^$/22/'
}

# enable_ufw turns on ufw with only SSH, HTTP and HTTPS allowed. SSH ports are
# allowed before enabling, so the current session and future logins keep working.
enable_ufw() {
	ports=$(ssh_ports)
	# Rules first, enable last: if anything fails, ufw stays off (no lockout).
	for p in $ports; do
		ufw allow "$p/tcp" comment 'SSH admin and git' >/dev/null || die "ufw: could not allow SSH port $p; firewall left off"
	done
	ufw allow 80/tcp comment 'HTTP for certificates and redirect' >/dev/null || die "ufw: could not allow port 80; firewall left off"
	ufw allow 443/tcp comment 'HTTPS web UI' >/dev/null || die "ufw: could not allow port 443; firewall left off"
	ufw default deny incoming >/dev/null || die "ufw: could not set the default policy; firewall left off"
	ufw default allow outgoing >/dev/null || die "ufw: could not set the default policy; firewall left off"
	ufw --force enable >/dev/null || die "ufw: could not enable the firewall"
	for p in $ports; do
		ufw status | grep -Eq "^$p/tcp[[:space:]].*ALLOW" || die "ufw: SSH port $p is not allowed after enabling; run 'ufw allow $p/tcp' now"
	done
	ok "ufw on: allowing SSH ($ports), 80 and 443; everything else incoming is blocked"
}

# open_firewall makes sure ports 80 and 443 (HTTPS and Let's Encrypt) are open.
# If ufw is installed but off, it offers to turn it on (see enable_ufw).
open_firewall() {
	if command -v ufw >/dev/null && ! ufw status 2>/dev/null | grep -q '^Status: active' &&
		! { command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; }; then
		if [ "${ENABLE_UFW:-}" = 1 ]; then
			enable_ufw
		elif [ "$INTERACTIVE" = 1 ] && yes_no "The ufw firewall is off. Turn it on, allowing only SSH (port $(ssh_ports)), 80 and 443?"; then
			enable_ufw
		else
			warn "no firewall is active. To turn on ufw later: ufw allow $(ssh_ports | sed 's/ /,/g')/tcp && ufw allow 80,443/tcp && ufw enable"
		fi
		return 0
	fi
	if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q '^Status: active'; then
		missing=""
		for p in 80 443; do
			ufw status | grep -Eq "^$p(/tcp)?[[:space:]].*ALLOW" || missing="$missing $p"
		done
		if [ -z "$missing" ]; then
			ok "ufw is on and allows 80 and 443"
		elif [ "$INTERACTIVE" = 1 ] && yes_no "ufw blocks port(s)$missing, needed for HTTPS and Let's Encrypt. Open them?"; then
			for p in $missing; do ufw allow "$p/tcp" >/dev/null; done
			ok "opened port(s)$missing in ufw"
		else
			warn "ufw blocks port(s)$missing; run: ufw allow 80,443/tcp"
		fi
	elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
		missing=""
		for svc in http https; do
			firewall-cmd --query-service="$svc" >/dev/null 2>&1 || missing="$missing $svc"
		done
		if [ -z "$missing" ]; then
			ok "firewalld allows http and https"
		elif [ "$INTERACTIVE" = 1 ] && yes_no "firewalld blocks$missing, needed for HTTPS and Let's Encrypt. Open?"; then
			for svc in $missing; do firewall-cmd --permanent --add-service="$svc" >/dev/null; done
			firewall-cmd --reload >/dev/null
			ok "opened$missing in firewalld"
		else
			warn "firewalld blocks$missing; run: firewall-cmd --permanent --add-service=http --add-service=https && firewall-cmd --reload"
		fi
	else
		info "no firewall found (ufw/firewalld); skipping"
	fi
}

# https_status checks once whether Caddy serves a publicly trusted certificate.
https_status() {
	if internal_domain "$DOMAIN"; then
		CERT_STATUS="local certificate (Caddy's internal CA; $DOMAIN is not a public name)"
		return 0
	fi
	if curl -fsS -o /dev/null --max-time 5 --resolve "$DOMAIN:443:127.0.0.1" "https://$DOMAIN/static/style.css" 2>/dev/null; then
		issuer=$(echo | openssl s_client -connect 127.0.0.1:443 -servername "$DOMAIN" 2>/dev/null |
			openssl x509 -noout -issuer 2>/dev/null | sed -n 's/.*O *= *\([^,]*\).*/\1/p')
		CERT_STATUS="active, issued by ${issuer:-a public CA}; renewed automatically"
		return 0
	fi
	CERT_STATUS="NOT issued yet. Check DNS and ports 80/443, then: journalctl -u caddy | grep -iE 'acme|challenge|certificate'. Caddy keeps retrying."
	return 1
}

# wait_for_certificate polls https_status for up to $1 seconds.
CERT_STATUS="unknown"
wait_for_certificate() {
	i=0
	while ! https_status; do
		[ "$i" -ge "$1" ] && spin_end && return 1
		[ "$i" -eq 0 ] && info "waiting for the HTTPS certificate (Caddy requests it from Let's Encrypt automatically)..."
		for _ in 1 2 3 4 5; do
			spin "waiting for the certificate" "$i"
			sleep 1
			i=$((i + 1))
		done
	done
	spin_end
}

# SHA-256 of the official Anubis release packages (from the GitHub release).
anubis_sha256() {
	case "$ANUBIS_VERSION/$1" in
	1.27.0/anubis_1.27.0_amd64.deb) echo 13457d54f6d190d38ae23e0a3c206d20b97a855cfa88d9081b90aec316c1af99 ;;
	1.27.0/anubis_1.27.0_arm64.deb) echo d185d84d1bebee21d55e9d56f76533858dbb4a2102a17c85265d94bdc46b69a3 ;;
	1.27.0/anubis-1.27.0-1.x86_64.rpm) echo d95db3c2e1c114cc604132bf5f4192d3ddd7622f35d4b4c006b5462cc0a13b8d ;;
	1.27.0/anubis-1.27.0-1.aarch64.rpm) echo de63cc8f1a10a2174c7a6bfe6eb3c8791724436d2f979c1163c08902000ddfb6 ;;
	*) echo "${ANUBIS_SHA256:-}" ;;
	esac
}

anubis_ok() {
	command -v anubis >/dev/null && [ "$(anubis --version 2>/dev/null | grep -o '[0-9][0-9.]*' | head -1)" = "$ANUBIS_VERSION" ]
}

# ---------------------------------------------------------------- start

[ "$(id -u)" -eq 0 ] || die "run as root"
command -v systemctl >/dev/null || die "systemd is required"
[ -x "$BIN" ] || die "gitserver binary not found at $BIN (set BIN=...)"
: >>"$LOGFILE"
printf '\n=== %s gitserver install.sh\n' "$(date -Is)" >>"$LOGFILE"

if command -v apt-get >/dev/null; then
	PKG=deb
	export DEBIAN_FRONTEND=noninteractive
elif command -v dnf >/dev/null; then
	PKG=rpm
else
	die "unsupported distribution (need apt or dnf)"
fi
case "$(uname -m)" in
x86_64) DEB_ARCH=amd64 RPM_ARCH=x86_64 ;;
aarch64) DEB_ARCH=arm64 RPM_ARCH=aarch64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac

# Settings of an existing installation.
PREV_CADDY=/etc/caddy/gitserver.caddy
PREV_UNIT=/etc/systemd/system/gitserver.service
PREV_DOMAIN=$(sed -n 's/^\([A-Za-z0-9.-]*\) {$/\1/p' "$PREV_CADDY" 2>/dev/null | head -1)
PREV_EMAIL=$(sed -n 's/^[[:space:]]*tls \([^ ]*@[^ ]*\)$/\1/p' "$PREV_CADDY" 2>/dev/null | head -1)
PREV_SITE=$(sed -n 's/.* -site "\([^"]*\)".*/\1/p' "$PREV_UNIT" 2>/dev/null | head -1)
PREV_VERSION=$(/usr/local/bin/gitserver version 2>/dev/null | awk '{print $2}')

MODE=install
if [ -n "$PREV_DOMAIN" ] && [ -f "$PREV_UNIT" ] && [ -x /usr/local/bin/gitserver ] && [ "${RECONFIGURE:-0}" != 1 ]; then
	if [ -z "${DOMAIN:-}" ] || [ "$DOMAIN" = "$PREV_DOMAIN" ]; then
		MODE=update
		DOMAIN=$PREV_DOMAIN
		SITE_NAME=${SITE_NAME:-${PREV_SITE:-$DOMAIN}}
	elif [ "$INTERACTIVE" = 1 ]; then
		yes_no "gitserver is installed for $PREV_DOMAIN. Move it to $DOMAIN?" || die "cancelled"
	fi
fi

if [ "$MODE" = install ]; then
	if [ -z "${DOMAIN:-}" ]; then
		[ "$INTERACTIVE" = 1 ] || die "set DOMAIN, e.g. DOMAIN=git.example.com"
		bold "gitserver installer: web UI + git over SSH, with Caddy (HTTPS) and Anubis"
		DOMAIN=$(ask "Domain name for this server (DNS must point here), e.g. git.example.com" "$PREV_DOMAIN")
		SITE_NAME=${SITE_NAME:-$(ask "Site name shown in the header" "${PREV_SITE:-$DOMAIN}")}
		if ! internal_domain "$DOMAIN" && [ -z "${ACME_EMAIL+set}" ]; then
			ACME_EMAIL=$(ask "Email for Let's Encrypt expiry notices (optional, Enter to skip)" "$PREV_EMAIL")
		fi
		echo
		echo "  Web UI:  https://$DOMAIN   (HTTPS certificate from Let's Encrypt, automatic)"
		echo "  Git:     git@$DOMAIN:~USER/REPO"
		if ! check_dns; then
			yes_no "DNS does not look ready. Continue anyway?" || die "cancelled"
		fi
		yes_no "Continue?" || die "cancelled"
		echo
	else
		check_dns || true
	fi
fi
SITE_NAME=${SITE_NAME:-$DOMAIN}
ACME_EMAIL=${ACME_EMAIL-$PREV_EMAIL}
internal_domain "$DOMAIN" && ACME_EMAIL="" # local CA, no Let's Encrypt account
if [ -n "$ACME_EMAIL" ]; then
	printf '%s' "$ACME_EMAIL" | grep -Eq '^[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$' || die "invalid ACME_EMAIL"
fi
printf '%s' "$DOMAIN" | grep -Eq '^[A-Za-z0-9.-]+$' || die "invalid DOMAIN"
printf '%s' "$SITE_NAME" | grep -Eq '^[A-Za-z0-9 ._-]+$' || die "SITE_NAME may only contain letters, digits, space, . _ -"

NEW_VERSION=$("$BIN" version 2>/dev/null | awk '{print $2}')
NEED_PACKAGES=1
if command -v git >/dev/null && command -v caddy >/dev/null && command -v curl >/dev/null && command -v openssl >/dev/null && anubis_ok; then
	NEED_PACKAGES=0
fi

if [ "$MODE" = update ]; then
	TOTAL=8
	[ "$NEED_PACKAGES" = 1 ] && TOTAL=9
	bold "Updating gitserver on $DOMAIN (${PREV_VERSION:-?} -> ${NEW_VERSION:-?}); no questions. RECONFIGURE=1 changes settings."
else
	TOTAL=11
fi
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# ---------------------------------------------------------------- packages

if [ "$MODE" = install ] || [ "$NEED_PACKAGES" = 1 ]; then
	step "Packages (git, caddy, curl, openssl)"
	if [ "$PKG" = deb ]; then
		quiet apt-get update -q
		quiet apt-get install -y -q git curl openssl ca-certificates
		apt-get install -y -q caddy >>"$LOGFILE" 2>&1 || die "could not install caddy; see https://caddyserver.com/docs/install (log: $LOGFILE)"
	else
		quiet dnf install -y -q git curl openssl ca-certificates
		dnf install -y -q caddy >>"$LOGFILE" 2>&1 || die "could not install caddy (on RHEL enable EPEL or the caddy COPR first; log: $LOGFILE)"
	fi
	ok "git $(git --version | awk '{print $3}'), caddy $(caddy version 2>/dev/null | awk '{print $1}')"

	if anubis_ok; then
		ok "anubis $ANUBIS_VERSION already installed"
	else
		if [ "$PKG" = deb ]; then
			ASSET="anubis_${ANUBIS_VERSION}_${DEB_ARCH}.deb"
		else
			ASSET="anubis-${ANUBIS_VERSION}-1.${RPM_ARCH}.rpm"
		fi
		SUM=$(anubis_sha256 "$ASSET")
		[ -n "$SUM" ] || die "no checksum known for $ASSET; set ANUBIS_SHA256"
		curl -fsSL --proto '=https' -o "$TMP/$ASSET" \
			"https://github.com/TecharoHQ/anubis/releases/download/v${ANUBIS_VERSION}/${ASSET}" || die "could not download $ASSET"
		echo "$SUM  $TMP/$ASSET" | sha256sum -c - >/dev/null || die "checksum mismatch for $ASSET"
		if [ "$PKG" = deb ]; then
			quiet apt-get install -y -q "$TMP/$ASSET"
		else
			quiet dnf install -y -q "$TMP/$ASSET"
		fi
		ok "anubis $ANUBIS_VERSION installed (SHA-256 verified)"
		changed "Anubis package"
	fi
fi

SSHD=$(command -v sshd || echo /usr/sbin/sshd)
[ -x "$SSHD" ] || die "the OpenSSH server (sshd) is not installed"

# ---------------------------------------------------------------- user + key

if [ "$MODE" = install ]; then
	step "Service user 'git'"
	if ! id git >/dev/null 2>&1; then
		useradd --system --user-group --home-dir /var/lib/gitserver --shell /bin/sh git
		ok "created user git"
	else
		ok "user git exists"
	fi
fi
# sshd runs forced commands through the login shell, so git needs a real
# shell. It can never use it: every key gets a forced command (restrict).
id git >/dev/null 2>&1 || die "the git user is missing; run with RECONFIGURE=1"
usermod --shell /bin/sh git >/dev/null 2>&1 || true
# "*": no password can ever match, but the account is not "locked" (sshd
# rejects locked accounts even for public keys when PAM is disabled).
usermod -p '*' git >/dev/null 2>&1 || true
install -d -o git -g git -m 0700 /var/lib/gitserver
# Caddy and Anubis reach gitserver (and Caddy reaches Anubis) over Unix
# sockets that only this group may open; no other local user can connect.
if ! getent group gitserver-http >/dev/null; then
	groupadd --system gitserver-http
	changed "socket group"
fi

step "2FA encryption key"
install -d -o root -g root -m 0700 /etc/gitserver
if [ ! -f /etc/gitserver/secret.key ]; then
	(umask 077 && openssl rand -hex 32 >/etc/gitserver/secret.key)
	NEW_KEY=1
	ok "created /etc/gitserver/secret.key"
	changed "new 2FA key"
else
	same "/etc/gitserver/secret.key kept"
fi
chown root:root /etc/gitserver/secret.key
chmod 0600 /etc/gitserver/secret.key

# ---------------------------------------------------------------- gitserver

step "gitserver ${NEW_VERSION:-}"
CH_GITSERVER=0 CH_UNIT=0
if put "$BIN" /usr/local/bin/gitserver 0755; then
	ok "binary ${PREV_VERSION:+$PREV_VERSION -> }${NEW_VERSION:-installed}"
	changed "gitserver binary"
	CH_GITSERVER=1
else
	same "binary unchanged (${NEW_VERSION:-same build})"
fi
put "$HERE/gitserverctl" /usr/local/sbin/gitserverctl 0755 && changed "gitserverctl"
install -d /usr/local/share/doc/gitserver
[ -f "$HERE/../README.md" ] && { put "$HERE/../README.md" /usr/local/share/doc/gitserver/README.md 0644 || true; }
sed -e "s|git\.example\.com\"|$SITE_NAME\"|" -e "s|git\.example\.com|$DOMAIN|g" \
	"$HERE/gitserver.service" >"$TMP/gitserver.service"
if put "$TMP/gitserver.service" /etc/systemd/system/gitserver.service 0644; then
	ok "systemd unit updated"
	changed "systemd unit"
	CH_UNIT=1
else
	same "systemd unit unchanged"
fi
CH_SOCKET=0
if put "$HERE/gitserver.socket" /etc/systemd/system/gitserver.socket 0644; then
	ok "socket unit updated (/run/gitserver/http.sock, group gitserver-http only)"
	changed "socket unit"
	CH_UNIT=1 CH_SOCKET=1
else
	same "socket unit unchanged"
fi

# ---------------------------------------------------------------- anubis

step "Anubis (bot protection)"
CH_ANUBIS=0
install -d -m 0755 /etc/anubis
if put "$HERE/anubis.botPolicies.yaml" /etc/anubis/gitserver.botPolicies.yaml 0644; then
	ok "bot policy updated"
	changed "Anubis policy"
	CH_ANUBIS=1
else
	same "bot policy unchanged"
fi
if [ ! -f /etc/anubis/gitserver.env ]; then
	KEY=$(openssl rand -hex 32)
	(umask 077 && sed -e "s|git\.example\.com|$DOMAIN|g" -e "s|@KEY@|$KEY|" "$HERE/anubis.env" >/etc/anubis/gitserver.env)
	ok "configuration created"
	CH_ANUBIS=1
else
	same "configuration and signing key kept"
fi
# The socket settings are kept as in the template even in an existing file,
# so updated installs move from localhost ports to Unix sockets.
SOCKETS_SET=0
for key in BIND BIND_NETWORK SOCKET_MODE TARGET METRICS_BIND METRICS_BIND_NETWORK; do
	line=$(grep "^$key=" "$HERE/anubis.env") || die "anubis.env has no $key"
	set_env /etc/anubis/gitserver.env "$line" && SOCKETS_SET=1
done
if [ "$SOCKETS_SET" = 1 ]; then
	ok "listens on a Unix socket (/run/anubis/gitserver/anubis.sock)"
	changed "Anubis sockets"
	CH_ANUBIS=1
fi
chmod 0600 /etc/anubis/gitserver.env
install -d -m 0755 /etc/systemd/system/anubis@gitserver.service.d
if put "$HERE/anubis-gitserver.conf" /etc/systemd/system/anubis@gitserver.service.d/gitserver.conf 0644; then
	ok "runs with group gitserver-http (socket access)"
	changed "Anubis unit drop-in"
	CH_UNIT=1 CH_ANUBIS=1
fi

# ---------------------------------------------------------------- caddy

step "Caddy (HTTPS)"
CH_CADDY=0
install -d -o caddy -g caddy /var/log/caddy 2>/dev/null || install -d /var/log/caddy
if [ -n "$ACME_EMAIL" ]; then
	sed -e "s|git\.example\.com|$DOMAIN|g" -e "s|^[[:space:]]*# acme-email.*|\ttls $ACME_EMAIL|" "$HERE/gitserver.caddy" >"$TMP/gitserver.caddy"
else
	sed -e "s|git\.example\.com|$DOMAIN|g" -e '/^[[:space:]]*# acme-email/d' "$HERE/gitserver.caddy" >"$TMP/gitserver.caddy"
fi
install -d -m 0755 /etc/systemd/system/caddy.service.d
if put "$HERE/caddy-gitserver.conf" /etc/systemd/system/caddy.service.d/gitserver.conf 0644; then
	ok "runs with group gitserver-http (socket access)"
	changed "Caddy unit drop-in"
	CH_UNIT=1
fi
CADDYFILE=/etc/caddy/Caddyfile
cp -p "$PREV_CADDY" "$TMP/caddy.before" 2>/dev/null || true
cp -p "$CADDYFILE" "$TMP/Caddyfile.before" 2>/dev/null || true
if put "$TMP/gitserver.caddy" "$PREV_CADDY" 0644; then CH_CADDY=1; fi
# A Caddyfile written by this installer also moves Caddy's admin API from
# 127.0.0.1:2019, where any local user could rewrite Caddy's configuration,
# to a Unix socket only the caddy user can open (systemctl reload uses it).
OWN_CADDYFILE='{
	admin unix//run/caddy/admin.sock
}

import /etc/caddy/gitserver.caddy'
if [ ! -f "$CADDYFILE" ] || grep -q '/usr/share/caddy' "$CADDYFILE" ||
	[ "$(cat "$CADDYFILE")" = 'import /etc/caddy/gitserver.caddy' ]; then
	# Missing, the distribution's placeholder site, or written by an older
	# version of this installer: replace it.
	grep -q '/usr/share/caddy' "$CADDYFILE" 2>/dev/null && cp "$CADDYFILE" "$CADDYFILE.orig.$(date +%s)"
	printf '%s\n' "$OWN_CADDYFILE" >"$CADDYFILE"
	CH_CADDY=1
elif [ "$(cat "$CADDYFILE")" = "$OWN_CADDYFILE" ]; then
	: # ours and current
elif ! grep -q 'import /etc/caddy/gitserver.caddy' "$CADDYFILE"; then
	cp "$CADDYFILE" "$CADDYFILE.orig.$(date +%s)"
	printf '\nimport /etc/caddy/gitserver.caddy\n' >>"$CADDYFILE"
	CH_CADDY=1
fi
if [ "$CH_CADDY" = 1 ]; then
	if ! caddy validate --adapter caddyfile --config "$CADDYFILE" >>"$LOGFILE" 2>&1; then
		[ -f "$TMP/caddy.before" ] && cp -p "$TMP/caddy.before" "$PREV_CADDY"
		[ -f "$TMP/Caddyfile.before" ] && cp -p "$TMP/Caddyfile.before" "$CADDYFILE"
		die "the new Caddy config is invalid; the old one was restored (log: $LOGFILE)"
	fi
	ok "site config for $DOMAIN updated${ACME_EMAIL:+ (ACME email $ACME_EMAIL)}"
	changed "Caddy config"
else
	same "site config unchanged"
fi
if ! grep -q '^[[:space:]]*admin[[:space:]]*unix//' "$CADDYFILE"; then
	warn "Caddy's admin API listens on 127.0.0.1:2019, where any local user can change Caddy's configuration. Add a global options block at the top of $CADDYFILE: { admin unix//run/caddy/admin.sock }"
fi

# ---------------------------------------------------------------- openssh

step "OpenSSH (git over SSH, user 'git' only)"
SSHD_CONF=/etc/ssh/sshd_config
DROPIN=/etc/ssh/sshd_config.d/50-gitserver.conf
cp -p "$SSHD_CONF" "$TMP/sshd_config.before"
CH_SSH=0
if grep -Eq '^[[:space:]]*Include[[:space:]]+/etc/ssh/sshd_config\.d/\*\.conf' "$SSHD_CONF"; then
	cp -p "$DROPIN" "$TMP/dropin.before" 2>/dev/null || true
	if put "$HERE/sshd-gitserver.conf" "$DROPIN" 0644; then CH_SSH=1; fi
else
	# No drop-in directory: keep the Match block at the end of sshd_config.
	DROPIN=""
	sed '/^# BEGIN gitserver$/,/^# END gitserver$/d' "$SSHD_CONF" >"$TMP/sshd_config.new"
	{ echo '# BEGIN gitserver'; cat "$HERE/sshd-gitserver.conf"; echo '# END gitserver'; } >>"$TMP/sshd_config.new"
	if ! cmp -s "$TMP/sshd_config.new" "$SSHD_CONF"; then
		cat "$TMP/sshd_config.new" >"$SSHD_CONF"
		CH_SSH=1
	fi
fi
if [ "$CH_SSH" = 1 ]; then
	if ! "$SSHD" -t 2>>"$LOGFILE"; then
		cp -p "$TMP/sshd_config.before" "$SSHD_CONF"
		if [ -n "$DROPIN" ]; then
			if [ -f "$TMP/dropin.before" ]; then cp -p "$TMP/dropin.before" "$DROPIN"; else rm -f "$DROPIN"; fi
		fi
		die "sshd rejected the new configuration; it was rolled back and SSH is unchanged"
	fi
	for unit in ssh sshd; do
		systemctl try-reload-or-restart "$unit.service" 2>/dev/null || true
	done
	ok "sshd config updated and reloaded (your own logins are not affected)"
	changed "sshd config"
else
	same "sshd config unchanged"
fi
if command -v getenforce >/dev/null && [ "$(getenforce)" = Enforcing ]; then
	warn "SELinux is enforcing. If 'ssh git@$DOMAIN' is denied, check: ausearch -m avc -ts recent"
fi

# ---------------------------------------------------------------- firewall

if [ "$MODE" = install ]; then
	step "Firewall"
	open_firewall
fi

# ---------------------------------------------------------------- system protection

# Automatic security updates and fail2ban for SSH. Both are left alone if
# the admin turned them off (AUTO_UPDATES=0, FAIL2BAN=0); on an update they
# are only installed if missing.
step "Security updates and SSH protection"
if [ "$PKG" = deb ]; then
	if [ "${AUTO_UPDATES:-1}" = 1 ]; then
		if ! deb_installed unattended-upgrades; then
			quiet apt-get install -y -q unattended-upgrades
			changed "automatic security updates"
		fi
		UU=""
		eval "$(apt-config shell UU APT::Periodic::Unattended-Upgrade)"
		case "$UU" in
		1) ok "automatic security updates on (unattended-upgrades, daily, no automatic reboots)" ;;
		0) warn "automatic updates are turned off in /etc/apt/apt.conf.d (APT::Periodic::Unattended-Upgrade \"0\"); left as they are" ;;
		*)
			printf 'APT::Periodic::Update-Package-Lists "1";\nAPT::Periodic::Unattended-Upgrade "1";\n' >/etc/apt/apt.conf.d/20auto-upgrades
			ok "turned on automatic security updates (unattended-upgrades, daily, no automatic reboots)"
			changed "automatic security updates"
			;;
		esac
	else
		info "automatic security updates skipped (AUTO_UPDATES=0)"
	fi
	if [ "${FAIL2BAN:-1}" = 1 ]; then
		if ! deb_installed fail2ban; then
			quiet apt-get install -y -q fail2ban python3-systemd nftables
			changed "fail2ban"
		fi
		sed "s|@PORTS@|$(ssh_ports | tr ' ' ',')|g" "$HERE/fail2ban-gitserver.conf" >"$TMP/fail2ban.conf"
		CH_F2B=0
		put "$TMP/fail2ban.conf" /etc/fail2ban/jail.d/gitserver.conf 0644 && CH_F2B=1
		systemctl enable fail2ban.service >>"$LOGFILE" 2>&1 || true
		if [ "$CH_F2B" = 1 ] || ! systemctl is-active --quiet fail2ban.service; then
			systemctl restart fail2ban.service >>"$LOGFILE" 2>&1 || true
		fi
		i=0
		until fail2ban-client status sshd >/dev/null 2>&1; do
			i=$((i + 1))
			[ "$i" -ge 10 ] && break
			spin "waiting for fail2ban to start" "$i"
			sleep 1
		done
		spin_end
		if fail2ban-client status sshd >/dev/null 2>&1; then
			ok "fail2ban guards SSH (port $(ssh_ports | tr ' ' ',')): 5 failed logins in 10 minutes block an address for an hour"
			[ "$CH_F2B" = 1 ] && changed "fail2ban jail"
		else
			warn "fail2ban did not start; SSH is not protected against password guessing. See: journalctl -u fail2ban"
		fi
	else
		info "fail2ban skipped (FAIL2BAN=0)"
	fi
else
	# Not set up automatically: untested on Fedora/RHEL, where package
	# names and defaults differ between releases.
	info "automatic updates and fail2ban are not set up automatically on Fedora/RHEL; to do it yourself:"
	info "  dnf install dnf-automatic && systemctl enable --now dnf-automatic-install.timer"
	info "  dnf install fail2ban && systemctl enable --now fail2ban   (then enable the [sshd] jail)"
fi

# ---------------------------------------------------------------- services

step "Services"
[ "$CH_UNIT" = 1 ] && systemctl daemon-reload
systemctl enable gitserver.socket gitserver.service anubis@gitserver.service caddy.service >>"$LOGFILE" 2>&1
RESTARTED=""
restart() { # restart UNIT REASON
	systemctl restart "$1" || die "$1 failed to restart; see: journalctl -u $1"
	ok "restarted $1 ($2)"
	RESTARTED="$RESTARTED, ${1%.service}"
}
# has_group UNIT GROUP: the running process of UNIT is a member of GROUP.
has_group() {
	pid=$(systemctl show -p MainPID --value "$1" 2>/dev/null)
	gid=$(getent group "$2" | cut -d: -f3)
	[ -n "$gid" ] && [ "${pid:-0}" != 0 ] &&
		awk -v g="$gid" '/^(Gid|Groups):/ { for (i = 2; i <= NF; i++) if ($i == g) found = 1 } END { exit !found }' "/proc/$pid/status"
}
# The decisions below look at what is running, not only at what this run
# changed, so re-running after an interrupted update finishes the job.
#
# systemd creates gitserver's socket only while gitserver is stopped (older
# versions listened on 127.0.0.1:8080 themselves), and a changed socket unit
# only applies once the socket is recreated. The loop starts gitserver again.
if [ "$CH_SOCKET" = 1 ] || ! systemctl is-active --quiet gitserver.socket; then
	systemctl stop gitserver.service 2>>"$LOGFILE"
	systemctl restart gitserver.socket || die "gitserver.socket failed to start; see: journalctl -u gitserver.socket"
	ok "started gitserver.socket (/run/gitserver/http.sock)"
	RESTARTED="$RESTARTED, gitserver.socket"
fi
for unit in gitserver.service anubis@gitserver.service caddy.service; do
	if ! systemctl is-active --quiet "$unit"; then
		systemctl start "$unit" || die "$unit failed to start; see: journalctl -u $unit"
		ok "started $unit"
		RESTARTED="$RESTARTED, ${unit%.service}"
		continue
	fi
	case "$unit" in
	gitserver.service)
		if [ "$MODE" = install ]; then
			restart "$unit" "install"
		elif [ "$CH_GITSERVER" = 1 ] || [ "$CH_UNIT" = 1 ] || [ "${NEW_KEY:-0}" = 1 ]; then
			restart "$unit" "new version"
		else
			same "gitserver.service running, not restarted"
		fi
		;;
	anubis@gitserver.service)
		if [ "$MODE" = install ]; then
			restart "$unit" "install"
		elif [ ! -S /run/anubis/gitserver/anubis.sock ]; then
			restart "$unit" "Unix socket"
		elif [ "$CH_ANUBIS" = 1 ]; then
			restart "$unit" "new settings"
		else
			same "anubis running, not restarted"
		fi
		;;
	caddy.service)
		if ! has_group caddy.service gitserver-http; then
			# A reload keeps the old process, which lacks the socket group.
			restart "$unit" "socket access"
		elif grep -q 'admin unix//run/caddy/admin.sock' "$CADDYFILE" && [ ! -S /run/caddy/admin.sock ]; then
			# A reload would look for the admin API on the new socket, which
			# the running process does not have yet.
			restart "$unit" "admin API on a Unix socket"
		elif [ "$MODE" = install ] || [ "$CH_CADDY" = 1 ]; then
			systemctl reload caddy.service || die "caddy reload failed; see: journalctl -u caddy"
			ok "reloaded caddy.service (new config, no downtime)"
			RESTARTED="$RESTARTED, caddy (reload)"
		else
			same "caddy running, not reloaded"
		fi
		;;
	esac
done
sleep 1
STATUS=""
for unit in gitserver anubis@gitserver caddy; do
	systemctl is-active --quiet "$unit" || die "$unit is not running; see: journalctl -u $unit"
	STATUS="$STATUS $unit ✓"
done
i=0
until curl -fsS -o /dev/null --max-time 3 --unix-socket /run/gitserver/http.sock http://localhost/static/style.css 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -ge 15 ] && spin_end && die "gitserver does not answer on /run/gitserver/http.sock; see: journalctl -u gitserver"
	spin "waiting for gitserver to answer" "$i"
	sleep 1
done
spin_end
# Anubis creates its socket shortly after starting; any HTTP answer will do
# (it may be a challenge page).
i=0
until [ "$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 --unix-socket /run/anubis/gitserver/anubis.sock \
	-H 'X-Real-IP: 127.0.0.1' "http://$DOMAIN/" 2>/dev/null)" != 000 ]; do
	i=$((i + 1))
	[ "$i" -ge 15 ] && spin_end && die "Anubis does not answer on /run/anubis/gitserver/anubis.sock; see: journalctl -u anubis@gitserver"
	spin "waiting for Anubis to answer" "$i"
	sleep 1
done
spin_end
for sock in /run/gitserver/http.sock /run/anubis/gitserver/anubis.sock; do
	[ "$(stat -c %G:%a "$sock")" = gitserver-http:660 ] || warn "$sock is $(stat -c %U:%G/%a "$sock"), expected group gitserver-http, mode 660"
done
ok "all running:$STATUS"

# ---------------------------------------------------------------- https

step "HTTPS certificate"
if [ "$MODE" = install ] || [ "$CH_CADDY" = 1 ]; then
	wait_for_certificate "${CERT_WAIT_SECONDS:-120}" || true
else
	https_status || true
fi
case "$CERT_STATUS" in NOT*) warn "$CERT_STATUS" ;; *) ok "$CERT_STATUS" ;; esac

KEY_NOTE="Keep a copy of /etc/gitserver/secret.key somewhere safe (e.g. a password
manager), separate from the backups: 2FA logins need it, and backups alone
cannot be decrypted without it."
[ "${NEW_KEY:-0}" = 1 ] && KEY_NOTE="NEW: $KEY_NOTE"

# ---------------------------------------------------------------- summary (update)

if [ "$MODE" = update ]; then
	echo
	if [ -z "$CHANGED" ] && [ -z "$RESTARTED" ]; then
		bold "Up to date: nothing changed on $DOMAIN (${NEW_VERSION:-same version}); no services were restarted."
	elif [ -z "$CHANGED" ]; then
		bold "No files changed on $DOMAIN, but services were brought up to date: ${RESTARTED#, }."
	else
		bold "Updated $DOMAIN: ${CHANGED#, }."
	fi
	echo "  Version:  ${PREV_VERSION:-?} -> ${NEW_VERSION:-?}"
	echo "  Services:$STATUS"
	echo "  HTTPS:    $CERT_STATUS"
	[ "${NEW_KEY:-0}" = 1 ] && printf '\n%s\n' "$KEY_NOTE"
	exit 0
fi

# ---------------------------------------------------------------- first admin (install)

if [ "$INTERACTIVE" = 1 ] && [ -z "$(gitserverctl user list 2>/dev/null)" ]; then
	echo
	case "$(ask "There are no accounts yet. Create the first admin now? [Y/n]" "y")" in
	[Yy]*)
		ADMIN=$(ask "Admin username" "")
		echo "Paste the contents of ~/.ssh/id_ed25519.pub from YOUR computer when asked for the SSH key."
		gitserverctl user add -admin -issuer "$SITE_NAME" "$ADMIN" </dev/tty || warn "could not create the admin; run: sudo gitserverctl user add -admin NAME"
		;;
	esac
fi

if [ -n "$(gitserverctl user list 2>/dev/null)" ]; then
	STEP2="2. Admin account: done (see: sudo gitserverctl user list)."
else
	STEP2="2. Create the first admin. You will be asked for your SSH *public* key
     (the contents of ~/.ssh/id_ed25519.pub on your computer), a password,
     and to scan a TOTP QR code:
       sudo gitserverctl user add -admin YOURNAME"
fi

HOSTKEYS=$(for k in /etc/ssh/ssh_host_ed25519_key.pub /etc/ssh/ssh_host_ecdsa_key.pub /etc/ssh/ssh_host_rsa_key.pub; do
	[ -f "$k" ] && ssh-keygen -lf "$k" | awk '{print "     " $2 " " $NF}'
done)

cat <<EOF

Done. gitserver, Anubis and Caddy run in the background and start on boot.
  Web UI:  https://$DOMAIN   (Caddy -> Anubis -> gitserver)
  HTTPS:   $CERT_STATUS
  Git:     git@$DOMAIN:~USER/REPO   (OpenSSH, user "git")
  Manage:  sudo gitserverctl status | logs | restart | help
  Update:  run this installer again (quick, no questions)

Next steps:
  1. DNS for $DOMAIN must point here, with ports 22, 80 and 443 reachable.
  $STEP2
  3. Log in at https://$DOMAIN, create repositories, invite people
     (Settings -> invites), then:
       git clone git@$DOMAIN:~YOURNAME/REPO
  4. Test SSH from your computer:  ssh git@$DOMAIN
     ("Hi ~YOURNAME! ... no shell access" means it works.)
     The server's SSH host key fingerprints are:
$HOSTKEYS

Logs: sudo gitserverctl logs   (git access log: journalctl -t gitserver-ssh)
Back up regularly: /var/lib/gitserver/repos and a database snapshot from
  sudo gitserverctl backup /var/lib/gitserver/backup-\$(date +%F).db
${KEY_NOTE}
EOF
