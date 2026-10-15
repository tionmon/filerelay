#!/usr/bin/env bash
# filerelay one-click installer for Linux (systemd; amd64, arm64, armv6/7).
#
# build.sh turns this file into dist/filerelay-install.sh with the binaries
# embedded. Run that on server C:
#
#   sudo bash filerelay-install.sh                               # HTTP on :8080
#   sudo bash filerelay-install.sh --domain relay.example.com    # HTTPS, automatic certificate
#
# Running it again upgrades in place and keeps tokens and settings.

# Re-run under bash if started with sh/dash (POSIX syntax until this point).
if [ -z "${BASH_VERSION:-}" ]; then exec bash "$0" "$@"; fi
set -euo pipefail

PAYLOAD_LINE=__PAYLOAD_LINE__
VERSION=__VERSION__

# FILERELAY_ROOT installs under a prefix instead of / (used for testing).
ROOT="${FILERELAY_ROOT:-}"
BIN="$ROOT/usr/local/bin/filerelay"
ENVF="$ROOT/etc/filerelay.env"
UNIT="$ROOT/etc/systemd/system/filerelay.service"
LOGF="$ROOT/var/log/filerelay.log"
SVC_USER=filerelay

usage() {
	cat <<'EOF'
Usage: sudo bash filerelay-install.sh [options]

Install / upgrade (default):
  --domain NAME        serve HTTPS on 443 with a free Let's Encrypt certificate
                       (DNS for NAME must point here; ports 80 and 443 must be open)
  --email ADDR         contact address for Let's Encrypt (optional)
  --port N             listen port (default 8080, or 443 with --domain)
  --public-url URL     URL servers A and B use to reach this relay, if it differs
                       from what they connect to (e.g. behind NAT or a proxy)
  --ttl DURATION       delete files this long after upload, e.g. 24h, 168h; 0 = never (default 72h)
  --max-upload SIZE    max size per file, e.g. 50G; 0 = unlimited (default 0)
  --data DIR           data directory (default /var/lib/filerelay)
  --no-read-token      don't create a separate read-only token
  --reset-tokens       generate new tokens (old ones stop working)
  --no-firewall        don't open ports in ufw / firewalld
  --binary FILE        install this filerelay binary instead of the embedded one

Other commands:
  --status             show service state, URL, tokens and client commands
  --uninstall          stop and remove the service and binary (keeps data and config)
  --uninstall --purge  also delete all relayed files, config and the service user
  -h, --help           show this help
EOF
	exit "${1:-0}"
}

if [ -t 1 ]; then B=$'\e[1m' G=$'\e[32m' Y=$'\e[33m' R=$'\e[31m' D=$'\e[2m' N=$'\e[0m'; else B="" G="" Y="" R="" D="" N=""; fi
step() { printf '%s==>%s %s\n' "$G" "$N" "$*"; }
warn() { printf '%swarning:%s %s\n' "$Y" "$N" "$*" >&2; }
die() { printf '%serror:%s %s\n' "$R" "$N" "$*" >&2; exit 1; }
have() { command -v "$1" >/dev/null 2>&1; }

# ---- options ----
ACTION=install PURGE=0 NO_READ_TOKEN=0 RESET_TOKENS=0 NO_FIREWALL=0
OPT_DOMAIN="" OPT_EMAIL="" OPT_PORT="" OPT_PUBLIC_URL="" OPT_TTL="" OPT_MAX="" OPT_DATA="" OPT_BINARY=""
need() { [ $# -ge 2 ] && [ -n "$2" ] || die "$1 needs a value"; }
while [ $# -gt 0 ]; do
	case "$1" in
	--domain) need "$@"; OPT_DOMAIN="$2"; shift ;;
	--email) need "$@"; OPT_EMAIL="$2"; shift ;;
	--port) need "$@"; OPT_PORT="$2"; shift ;;
	--public-url) need "$@"; OPT_PUBLIC_URL="${2%/}"; shift ;;
	--ttl) need "$@"; OPT_TTL="$2"; shift ;;
	--max-upload) need "$@"; OPT_MAX="$2"; shift ;;
	--data) need "$@"; OPT_DATA="$2"; shift ;;
	--binary) need "$@"; OPT_BINARY="$2"; shift ;;
	--no-read-token) NO_READ_TOKEN=1 ;;
	--reset-tokens) RESET_TOKENS=1 ;;
	--no-firewall) NO_FIREWALL=1 ;;
	--status) ACTION=status ;;
	--uninstall) ACTION=uninstall ;;
	--purge) PURGE=1 ;;
	-h | --help) usage ;;
	*) printf 'unknown option: %s\n\n' "$1" >&2; usage 2 ;;
	esac
	shift
done
if [ -n "$OPT_PORT" ]; then
	[[ "$OPT_PORT" =~ ^[0-9]+$ ]] && ((OPT_PORT >= 1 && OPT_PORT <= 65535)) || die "invalid --port: $OPT_PORT"
fi
if [ -n "$OPT_DOMAIN" ]; then
	[[ "$OPT_DOMAIN" =~ ^[A-Za-z0-9.-]+$ ]] || die "invalid --domain: $OPT_DOMAIN"
fi
if [ -n "$OPT_DATA" ]; then
	[[ "$OPT_DATA" == /* ]] || die "--data must be an absolute path"
fi

has_systemd() { [ -d "$ROOT/run/systemd/system" ] && have systemctl; }

# ---- config file ----
# Current settings, loaded from /etc/filerelay.env when it exists.
LISTEN="" DATA="" TOKEN="" READ_TOKEN="" TTL="" MAX_UPLOAD="" PUBLIC_URL="" DOMAIN="" ACME_EMAIL=""
EXTRA_LINES=""
KNOWN_KEYS="LISTEN DATA TOKEN READ_TOKEN TTL MAX_UPLOAD PUBLIC_URL DOMAIN ACME_EMAIL"

load_env() {
	local line k v
	[ -f "$ENVF" ] || return 1
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in
		RELAY_*=*)
			k="${line%%=*}" k="${k#RELAY_}" v="${line#*=}"
			v="${v%\"}" v="${v#\"}"
			if [[ " $KNOWN_KEYS " == *" $k "* ]]; then
				printf -v "$k" '%s' "$v"
			else
				EXTRA_LINES+="$line"$'\n'
			fi
			;;
		"" | "#"*) ;;
		*) EXTRA_LINES+="$line"$'\n' ;;
		esac
	done <"$ENVF"
}

write_env() {
	mkdir -p "$(dirname "$ENVF")"
	local tmp="$ENVF.tmp"
	(
		umask 077
		cat >"$tmp" <<EOF
# filerelay configuration. Edit, then: systemctl restart filerelay
# All options: filerelay -h
RELAY_LISTEN=$LISTEN
RELAY_DATA=$DATA
RELAY_TOKEN=$TOKEN
RELAY_READ_TOKEN=$READ_TOKEN
RELAY_TTL=$TTL
RELAY_MAX_UPLOAD=$MAX_UPLOAD
RELAY_PUBLIC_URL=$PUBLIC_URL
RELAY_DOMAIN=$DOMAIN
RELAY_ACME_EMAIL=$ACME_EMAIL
EOF
		[ -z "$EXTRA_LINES" ] || printf '%s' "$EXTRA_LINES" >>"$tmp"
	)
	mv -f "$tmp" "$ENVF"
	chmod 600 "$ENVF"
}

gen_token() {
	local t
	t=$(head -c 24 /dev/urandom | base64 | tr '+/' '-_' | tr -d '=\n')
	[ ${#t} -ge 30 ] || die "could not generate a random token"
	printf '%s' "$t"
}

port_of() { local p="${1##*:}"; printf '%s' "${p:-80}"; }

detect_ip() {
	local ip=""
	if have ip; then
		ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") { print $(i + 1); exit }}')
	fi
	[ -n "$ip" ] || ip=$(hostname -I 2>/dev/null | awk '{print $1}') || true
	printf '%s' "${ip:-SERVER-IP}"
}

is_private_ip() {
	case "$1" in
	10.* | 192.168.* | 127.* | 172.1[6-9].* | 172.2[0-9].* | 172.3[01].* | 100.6[4-9].* | 100.[7-9][0-9].* | 100.1[01][0-9].* | 100.12[0-7].*) return 0 ;;
	esac
	return 1
}

base_url() {
	local port
	port=$(port_of "$LISTEN")
	if [ -n "$PUBLIC_URL" ]; then
		printf '%s' "$PUBLIC_URL"
	elif [ -n "$DOMAIN" ]; then
		[ "$port" = 443 ] && printf 'https://%s' "${DOMAIN%%,*}" || printf 'https://%s:%s' "${DOMAIN%%,*}" "$port"
	else
		printf 'http://%s:%s' "$(detect_ip)" "$port"
	fi
}

# ---- binary ----
detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	armv6* | armv7* | armhf) echo arm ;;
	*) die "unsupported CPU architecture: $(uname -m) (supported: x86_64, aarch64, armv6/7)" ;;
	esac
}

sha256_of() {
	if have sha256sum; then sha256sum "$1" | awk '{print $1}'
	elif have shasum; then shasum -a 256 "$1" | awk '{print $1}'
	else openssl dgst -sha256 "$1" | awk '{print $NF}'; fi
}

# Puts the binary for this machine at $BIN.new.
stage_binary() {
	local arch name src="" self want got
	arch=$(detect_arch)
	name="filerelay-linux-$arch"
	mkdir -p "$(dirname "$BIN")"
	if [ -n "$OPT_BINARY" ]; then
		src="$OPT_BINARY"
		[ -f "$src" ] || die "no such file: $src"
	elif [[ "$PAYLOAD_LINE" =~ ^[0-9]+$ ]]; then
		self="${BASH_SOURCE[0]}"
		[ -f "$self" ] || die "run this installer from a file (sudo bash filerelay-install.sh), not through a pipe"
		STAGE_TMP=$(mktemp -d)
		trap 'rm -rf "$STAGE_TMP"' EXIT
		tail -n +"$PAYLOAD_LINE" "$self" | tar -xzf - -C "$STAGE_TMP" ||
			die "the embedded payload is damaged; download the installer again (copy it in binary mode)"
		src="$STAGE_TMP/$name"
		[ -f "$src" ] || die "this installer has no binary for $arch"
		want=$(awk -v n="$name" '$2 == n || $2 == "*" n {print $1}' "$STAGE_TMP/SHA256SUMS")
		got=$(sha256_of "$src")
		[ -n "$want" ] && [ "$want" = "$got" ] || die "checksum mismatch for embedded $name; download the installer again"
	else
		# Unbundled installer (deploy/install.sh): look next to it and in ../dist.
		self="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
		for c in "$self/$name" "$self/../dist/$name"; do
			[ -f "$c" ] && src="$c" && break
		done
		[ -n "$src" ] || die "no embedded binary; build dist/filerelay-install.sh with ./build.sh, or pass --binary FILE"
	fi
	install -m 0755 "$src" "$BIN.new"
	if [ -z "$ROOT" ]; then
		"$BIN.new" -version >/dev/null 2>&1 || { rm -f "$BIN.new"; die "$src does not run on this machine ($(uname -m))"; }
	fi
}

# ---- system ----
ensure_user() {
	[ -z "$ROOT" ] || return 0
	id "$SVC_USER" >/dev/null 2>&1 && return 0
	local nologin
	nologin=$(command -v nologin || echo /bin/false)
	if have useradd; then
		useradd --system --no-create-home --home-dir "$DATA" --shell "$nologin" "$SVC_USER"
	elif have adduser; then
		adduser -S -D -H -h "$DATA" -s "$nologin" "$SVC_USER"
	else
		die "cannot create user $SVC_USER (no useradd/adduser)"
	fi
}

write_unit() {
	local protect_home=true
	case "$DATA" in /home/* | /root/* | /run/user/*) protect_home=false ;; esac
	mkdir -p "$(dirname "$UNIT")"
	cat >"$UNIT" <<EOF
[Unit]
Description=filerelay file relay station
Documentation=file:///etc/filerelay.env
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SVC_USER
Group=$SVC_USER
EnvironmentFile=/etc/filerelay.env
ExecStart=/usr/local/bin/filerelay
Restart=always
RestartSec=3
LimitNOFILE=65536
# Allows binding ports 80/443 without running as root.
AmbientCapabilities=CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_BIND_SERVICE
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=$protect_home
PrivateTmp=true
ReadWritePaths=$DATA

[Install]
WantedBy=multi-user.target
EOF
}

stop_service() {
	if has_systemd; then
		systemctl stop filerelay >/dev/null 2>&1 || true
	elif have pkill; then
		pkill -x filerelay 2>/dev/null || true
	fi
}

start_service() {
	if has_systemd; then
		write_unit
		systemctl daemon-reload
		systemctl enable filerelay >/dev/null 2>&1
		systemctl restart filerelay
	else
		warn "systemd not found: starting filerelay in the background as root."
		warn "It will NOT start again after a reboot; add this to your init system:"
		warn "  set -a; . /etc/filerelay.env; set +a; exec /usr/local/bin/filerelay"
		mkdir -p "$(dirname "$LOGF")"
		(set -a; . "$ENVF"; set +a; nohup "$BIN" >>"$LOGF" 2>&1 &)
	fi
}

open_firewall() {
	[ "$NO_FIREWALL" = 0 ] || return 0
	local ports=("$(port_of "$LISTEN")") p
	[ -z "$DOMAIN" ] || ports+=(80)
	if have ufw && ufw status 2>/dev/null | grep -q "Status: active"; then
		for p in "${ports[@]}"; do ufw allow "$p/tcp" >/dev/null && step "ufw: opened port $p/tcp"; done
	elif have firewall-cmd && firewall-cmd --state >/dev/null 2>&1; then
		for p in "${ports[@]}"; do firewall-cmd --quiet --permanent --add-port="$p/tcp"; done
		firewall-cmd --quiet --reload && step "firewalld: opened port(s) ${ports[*]}/tcp"
	fi
}

# Waits until the service answers. With --domain this also obtains the
# certificate, which proves DNS and ports 80/443 work from the internet.
health_check() {
	local port url i
	port=$(port_of "$LISTEN")
	have curl || { warn "curl not found; skipping health check"; return 0; }
	if [ -n "$DOMAIN" ]; then
		local host="${DOMAIN%%,*}"
		step "requesting a Let's Encrypt certificate for $host (can take ~30 s)"
		url="https://$host:$port/healthz"
		for i in $(seq 1 12); do
			curl -fsS --max-time 10 --resolve "$host:$port:127.0.0.1" "$url" >/dev/null 2>&1 && { step "HTTPS is working"; return 0; }
			sleep 5
		done
		warn "HTTPS isn't working yet. Check that DNS for $host points to this server, that ports 80 and 443"
		warn "are open (also in your cloud provider's firewall), then see: journalctl -u filerelay -n 50"
		return 0
	fi
	for i in $(seq 1 20); do
		curl -fsS --max-time 2 "http://127.0.0.1:$port/healthz" >/dev/null 2>&1 && { step "service is up"; return 0; }
		sleep 0.5
	done
	warn "the service did not answer on port $port; see: journalctl -u filerelay -n 50"
}

print_summary() {
	local url ip
	url=$(base_url)
	cat <<EOF

${B}filerelay is running${N}   ${D}(version $("$BIN" -version 2>/dev/null | awk '{print $2}' || echo "$VERSION"))${N}

  Web UI:      ${B}$url${N}
  Read-write token (server A and the web UI):
               ${B}$TOKEN${N}
EOF
	if [ -n "$READ_TOKEN" ]; then
		cat <<EOF
  Read-only token (server B):
               ${B}$READ_TOKEN${N}
EOF
	fi
	cat <<EOF
  Files expire: $([ "$TTL" = 0 ] && echo never || echo "$TTL after upload")
  WebDAV:      $url/dav/   (any username, token as password)

${B}On server A (source):${N}
  curl -fsSL $url/relay.sh -o relay && chmod +x relay
  export RELAY_URL=$url RELAY_TOKEN=$TOKEN
  ./relay pack /path/to/project /migration

${B}On server B (destination):${N}
  curl -fsSL $url/relay.sh -o relay && chmod +x relay
  export RELAY_URL=$url RELAY_TOKEN=${READ_TOKEN:-$TOKEN}
  ./relay unpack /migration/project.tar.gz /path/to/target

${B}For an AI agent:${N} "Relay files through $url. Read $url/agent.md first."

${D}Config: /etc/filerelay.env · Data: $DATA · Logs: journalctl -u filerelay -f
Show this again: sudo bash $(basename "${BASH_SOURCE[0]}") --status${N}
EOF
	if [ -z "$DOMAIN" ]; then
		ip=$(detect_ip)
		printf '\n'
		warn "traffic (including the token) is unencrypted HTTP. Over the internet, re-run with"
		warn "  --domain your.domain   to get free automatic HTTPS."
		if [ -z "$PUBLIC_URL" ] && is_private_ip "$ip"; then
			warn "$ip is a private address. If A and B reach this server via a public IP or name,"
			warn "use that in RELAY_URL (or re-run with --public-url http://PUBLIC-IP:$(port_of "$LISTEN"))."
		fi
	fi
	printf '%sAlso allow the port(s) in your cloud provider firewall / security group if there is one.%s\n' "$D" "$N"
}

# ---- actions ----
do_install() {
	local fresh=0
	[ -n "$ROOT" ] || [ "$(id -u)" = 0 ] || die "run as root: sudo bash $(basename "${BASH_SOURCE[0]}")"
	load_env || fresh=1

	# Defaults for a new install, then explicit options on top.
	: "${LISTEN:=:8080}" "${DATA:=/var/lib/filerelay}" "${TTL:=72h}" "${MAX_UPLOAD:=0}"
	[ -z "$OPT_DATA" ] || DATA="$OPT_DATA"
	[ -z "$OPT_TTL" ] || TTL="$OPT_TTL"
	[ -z "$OPT_MAX" ] || MAX_UPLOAD="$OPT_MAX"
	[ -z "$OPT_EMAIL" ] || ACME_EMAIL="$OPT_EMAIL"
	[ -z "$OPT_PUBLIC_URL" ] || PUBLIC_URL="$OPT_PUBLIC_URL"
	if [ -n "$OPT_DOMAIN" ]; then
		DOMAIN="$OPT_DOMAIN"
		LISTEN=":${OPT_PORT:-443}"
	elif [ -n "$OPT_PORT" ]; then
		LISTEN=":$OPT_PORT"
	fi
	if [ -z "$TOKEN" ] || [ "$RESET_TOKENS" = 1 ]; then TOKEN=$(gen_token); fi
	if [ "$NO_READ_TOKEN" = 1 ]; then
		READ_TOKEN=""
	elif [ "$fresh" = 1 ] || [ "$RESET_TOKENS" = 1 ]; then
		READ_TOKEN=$(gen_token)
	fi

	if [ "$fresh" = 1 ]; then step "installing filerelay $VERSION"; else step "upgrading filerelay (keeping settings from /etc/filerelay.env)"; fi
	stage_binary
	stop_service
	mv -f "$BIN.new" "$BIN"
	ensure_user
	mkdir -p "$ROOT$DATA"
	[ -n "$ROOT" ] || chown -R "$SVC_USER:$SVC_USER" "$DATA"
	chmod 750 "$ROOT$DATA"
	write_env
	start_service
	open_firewall
	[ -n "$ROOT" ] || health_check
	print_summary
}

do_status() {
	load_env || die "filerelay is not installed (no /etc/filerelay.env)"
	if has_systemd; then
		printf 'service: %s\n' "$(systemctl is-active filerelay 2>/dev/null || true)"
	fi
	print_summary
}

do_uninstall() {
	[ -n "$ROOT" ] || [ "$(id -u)" = 0 ] || die "run as root"
	load_env || true
	: "${DATA:=/var/lib/filerelay}"
	if [ "$PURGE" = 1 ] && [ -t 0 ]; then
		read -r -p "Delete ALL relayed files in $DATA, the config and the tokens? [y/N] " ans
		[[ "$ans" =~ ^[Yy] ]] || die "aborted"
	fi
	if has_systemd; then
		systemctl disable --now filerelay >/dev/null 2>&1 || true
		rm -f "$UNIT"
		systemctl daemon-reload
	else
		stop_service
	fi
	rm -f "$BIN"
	step "removed the filerelay service and binary"
	if [ "$PURGE" = 1 ]; then
		rm -rf "${ROOT:?}$DATA" "$ENVF" "$LOGF"
		[ -n "$ROOT" ] || { id "$SVC_USER" >/dev/null 2>&1 && userdel "$SVC_USER" 2>/dev/null || true; }
		step "deleted $DATA, /etc/filerelay.env and the $SVC_USER user"
	else
		printf 'Kept %s and /etc/filerelay.env (run with --uninstall --purge to delete them).\n' "$DATA"
	fi
}

main() {
	case "$ACTION" in
	install) do_install ;;
	status) do_status ;;
	uninstall) do_uninstall ;;
	esac
}

main
exit
