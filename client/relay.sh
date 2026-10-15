#!/usr/bin/env bash
# relay - client for filerelay. Needs bash, curl, and sha256sum (or shasum).
#
#   export RELAY_URL=https://relay.example.com RELAY_TOKEN=...
#   relay push  <local-file-or-dir> [remote-dir]   upload (resumable, verified)
#   relay pull  <remote-path> [local-dir]          download file or dir (resumable, verified)
#   relay pack  <local-dir> [remote-dir]           tar.gz a directory, then upload it
#   relay unpack <remote.tar.gz> [local-dir]       download, verify, extract
#   relay ls [remote-dir] | sum <path> | rm <path> | info
#
# Env: RELAY_RETRIES (default 30), RELAY_CURL_OPTS (extra curl args, e.g. "-k").
set -euo pipefail

RELAY_URL="${RELAY_URL:-}"
RELAY_TOKEN="${RELAY_TOKEN:-}"
RETRIES="${RELAY_RETRIES:-30}"

log() { printf 'relay: %s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

usage() { sed -n '2,11s/^# \{0,1\}//p' "$0" >&2; exit 2; }

case "${1:-}" in "" | -h | --help | help) usage ;; esac
[ -n "$RELAY_URL" ] || die "RELAY_URL is not set"
[ -n "$RELAY_TOKEN" ] || die "RELAY_TOKEN is not set"
RELAY_URL="${RELAY_URL%/}"
command -v curl >/dev/null || die "curl is required"

TMPD=""
cleanup() { [ -z "$TMPD" ] || rm -rf "$TMPD"; }
trap cleanup EXIT

# Percent-encode a relay path, keeping "/" separators.
urlenc() {
	local LC_ALL=C s="$1" out="" c i
	for ((i = 0; i < ${#s}; i++)); do
		c="${s:i:1}"
		case "$c" in
		[a-zA-Z0-9._~/-]) out+="$c" ;;
		*) printf -v c '%%%02X' "'$c"; out+="$c" ;;
		esac
	done
	printf '%s' "$out"
}

sha256() {
	if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1
	else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

fsize() { wc -c <"$1" | tr -d ' '; }

# Join remote path parts into "/a/b/c".
rjoin() {
	local out="" p
	for p in "$@"; do
		p="${p#/}"; p="${p%/}"
		[ -n "$p" ] && out="$out/$p"
	done
	printf '%s' "${out:-/}"
}

# curl with auth; aborts transfers that stall below 1 KB/s for 60 s so the
# retry loop can take over on flaky links.
rc() {
	# shellcheck disable=SC2086
	curl -sS -f -g --connect-timeout 20 --speed-limit 1024 --speed-time 60 \
		-H "Authorization: Bearer $RELAY_TOKEN" ${RELAY_CURL_OPTS:-} "$@"
}

backoff() { local n=$(($1 * 2)); sleep $((n > 30 ? 30 : n)); }

# push_file <local-file> <remote-path>
push_file() {
	local f="$1" rp="$2" size sum ep off attempt=0
	[ -r "$f" ] || die "cannot read $f"
	size=$(fsize "$f")
	sum=$(sha256 "$f")
	ep="$RELAY_URL/api/files$(urlenc "$rp")"
	while :; do
		off=$(rc "$RELAY_URL/api/upload-status$(urlenc "$rp")?format=text" 2>/dev/null) || off=0
		[[ "$off" =~ ^[0-9]+$ ]] && ((off < size)) || off=0
		((off > 0)) && log "resuming $rp at $off/$size bytes"
		# File data goes through stdin so curl never has to open local paths
		# itself (avoids filename-encoding problems with some curl builds).
		if ((off > 0)); then
			tail -c +$((off + 1)) "$f" | rc -o /dev/null -T - 				-H "Content-Range: bytes $off-$((size - 1))/$size" -H "X-Checksum-Sha256: $sum" "$ep" && break
		else
			rc -o /dev/null -T - -H "X-Total-Size: $size" -H "X-Checksum-Sha256: $sum" "$ep" <"$f" && break
		fi
		attempt=$((attempt + 1))
		((attempt <= RETRIES)) || die "giving up on $f after $RETRIES retries"
		log "upload of $rp failed, retry $attempt/$RETRIES"
		backoff "$attempt"
	done
	log "pushed $f -> $rp ($size bytes, sha256 $sum)"
}

# push <local> [remote-dir]
cmd_push() {
	local src="${1:?usage: relay push <local-file-or-dir> [remote-dir]}" dest="${2:-/}" base rel n=0
	[ -e "$src" ] || die "no such file or directory: $src"
	src="${src%/}"
	base=$(basename "$src")
	if [ -f "$src" ]; then
		push_file "$src" "$(rjoin "$dest" "$base")"
		return
	fi
	[ -d "$src" ] || die "not a regular file or directory: $src"
	while IFS= read -r -d '' rel; do
		rel="${rel#./}"
		push_file "$src/$rel" "$(rjoin "$dest" "$base" "$rel")"
		n=$((n + 1))
	done < <(cd "$src" && find . -type f -print0)
	log "pushed $n file(s) from $src to $(rjoin "$dest" "$base")"
	log "note: empty dirs, symlinks and permissions are not transferred by push; use 'relay pack' to keep them"
}

# pull_file <remote-path> <local-file> <size> <sha256>
pull_file() {
	local rp="$1" lf="$2" size="$3" want="$4" part attempt=0 have
	part="$lf.relay-part"
	mkdir -p "$(dirname "$lf")"
	if [ -f "$lf" ] && [ "$(fsize "$lf")" = "$size" ] && [ "$(sha256 "$lf")" = "$want" ]; then
		log "up to date: $lf"
		return
	fi
	while :; do
		: >>"$part"
		have=$(fsize "$part")
		if ((have > size)); then rm -f "$part"; have=0; fi
		# Append to the partial file; the Range request continues where it stopped.
		if ((have == size)) || rc -C "$have" "$RELAY_URL/api/files$(urlenc "$rp")" >>"$part"; then
			if [ "$(sha256 "$part")" = "$want" ]; then
				mv -f "$part" "$lf"
				log "pulled $rp -> $lf ($size bytes, sha256 ok)"
				return
			fi
			log "checksum mismatch for $rp, downloading again"
			rm -f "$part"
		fi
		attempt=$((attempt + 1))
		((attempt <= RETRIES)) || die "giving up on $rp after $RETRIES retries"
		log "download of $rp failed, retry $attempt/$RETRIES"
		backoff "$attempt"
	done
}

# manifest <remote-path>: prints "size<TAB>sha256<TAB>relpath" lines.
# curl itself retries transient failures; a 4xx (e.g. not found) fails at once.
manifest() {
	rc --retry 5 --retry-delay 3 "$RELAY_URL/api/manifest$(urlenc "$1")?sha256=1&format=tsv" ||
		die "cannot list $1 on the relay"
}

# pull <remote-path> [local-dir]
cmd_pull() {
	local rp dest="${2:-.}" m size sum rel parent n=0
	rp=$(rjoin "${1:?usage: relay pull <remote-path> [local-dir]}")
	m=$(manifest "$rp")
	parent=$(dirname "$rp")
	[ "$rp" = "/" ] && parent="/"
	while IFS=$'\t' read -r size sum rel; do
		[ -n "$rel" ] || continue
		case "$rel" in /* | ../* | */../* | ..) die "refusing unsafe path from server: $rel" ;; esac
		pull_file "$(rjoin "$parent" "$rel")" "$dest/$rel" "$size" "$sum"
		n=$((n + 1))
	done <<<"$m"
	((n > 0)) || die "nothing to pull at $rp"
	log "pulled $n file(s) into $dest"
}

cmd_pack() {
	local src="${1:?usage: relay pack <local-dir> [remote-dir]}" dest="${2:-/}" base
	[ -d "$src" ] || die "not a directory: $src"
	src=$(cd "$src" && pwd)
	base=$(basename "$src")
	TMPD=$(mktemp -d)
	log "packing $src"
	tar -C "$(dirname "$src")" -czf "$TMPD/$base.tar.gz" "$base"
	push_file "$TMPD/$base.tar.gz" "$(rjoin "$dest" "$base.tar.gz")"
}

cmd_unpack() {
	local rp dest="${2:-.}" m size sum rel
	rp=$(rjoin "${1:?usage: relay unpack <remote.tar.gz> [local-dir]}")
	m=$(manifest "$rp")
	IFS=$'\t' read -r size sum rel <<<"$m"
	[ -n "$rel" ] || die "not found: $rp"
	TMPD=$(mktemp -d)
	pull_file "$rp" "$TMPD/archive" "$size" "$sum"
	mkdir -p "$dest"
	log "extracting into $dest"
	case "$rp" in
	*.tar.gz | *.tgz) tar -C "$dest" -xzf "$TMPD/archive" ;;
	*.tar.zst) tar -C "$dest" --zstd -xf "$TMPD/archive" ;;
	*) tar -C "$dest" -xf "$TMPD/archive" ;;
	esac
	log "unpacked $rp into $dest"
}

cmd="$1"
shift
case "$cmd" in
push) cmd_push "$@" ;;
pull) cmd_pull "$@" ;;
pack) cmd_pack "$@" ;;
unpack) cmd_unpack "$@" ;;
ls) rc "$RELAY_URL/api/ls$(urlenc "$(rjoin "${1:-/}")")?format=text" ;;
sum) rc "$RELAY_URL/api/sum$(urlenc "$(rjoin "${1:?usage: relay sum <path>}")")?format=text" ;;
rm) rc -X DELETE "$RELAY_URL/api/files$(urlenc "$(rjoin "${1:?usage: relay rm <path>}")")" && log "deleted $1" ;;
info) rc "$RELAY_URL/api/info" ;;
*) die "unknown command: $cmd (try: relay help)" ;;
esac
