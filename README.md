# filerelay

A small file relay station. When server **A** and server **B** can't talk to
each other reliably, run filerelay on a third server **C** that both reach
well: A pushes files to C, B pulls them from C.

```
  server A  ──push──▶  server C (filerelay)  ──pull──▶  server B
  (source)             web UI · REST · WebDAV           (destination)
```

One static binary (~7 MB, no dependencies) with:

- **REST API** built for scripts and AI agents: uploads that resume after a
  dropped connection, ranged downloads, SHA-256 verification, recursive
  manifests, tar.gz streaming.
- **WebDAV** at `/dav/` for rclone, davfs2, Finder, Windows Explorer.
- **Web UI**: browse, drag-and-drop files and folders, download, delete,
  copy pull commands.
- **`relay` client script** (bash + curl), served by the relay itself:
  `push`, `pull`, `pack`, `unpack` with automatic retries and checksum checks.
- **`/agent.md`**: plain-text instructions an AI agent can read to learn the
  relay on its own.
- **Read-write and read-only tokens**, and **automatic expiry** (default 72 h)
  so the relay doesn't fill up with old migration data.

## Design

| Concern | How it's handled |
|---|---|
| Flaky links | Uploads stage in `data/partial/`. An interrupted upload keeps its bytes; the client asks `/api/upload-status` for the offset and continues with `Content-Range`. Downloads support HTTP Range. The client script aborts stalled transfers (<1 KB/s for 60 s) and retries with backoff. |
| Integrity | The server computes SHA-256 while receiving. The client sends the expected hash (`X-Checksum-Sha256`); on mismatch the upload is discarded (422). Pulls verify every file against the server's hash before moving it into place. |
| Atomicity | A file appears to readers only after its last byte arrives and its checksum matches (staging file → `rename`). B can never pull a half-written file through the API. |
| Access | `RELAY_TOKEN` can do everything; `RELAY_READ_TOKEN` can only list and download (give that one to B). Tokens are accepted as Bearer tokens or as the HTTP Basic password, and the UI keeps them in an HttpOnly, SameSite=Strict cookie. Paths are confined to the data dir. |
| Cleanup | Files are deleted `RELAY_TTL` after their upload completed, along with emptied folders and abandoned partial uploads. |
| Metadata | Use `relay pack` / `unpack` (tar.gz) to keep permissions, symlinks and empty directories. `push`/`pull` move plain file contents. |

Layout on disk: `data/files/` (what clients see), `data/partial/` (in-progress
uploads), `data/token` (generated token, if you didn't set one).

## Deploy on server C

### Option 1: one-click installer (recommended)

`filerelay-install.sh` is a single file with the Linux binaries (amd64,
arm64, armv6/7) embedded; it needs no Go, git or further downloads. On server C:

```bash
curl -fLO https://github.com/tionmon/filerelay/releases/latest/download/filerelay-install.sh
sudo bash filerelay-install.sh                               # HTTP on port 8080
sudo bash filerelay-install.sh --domain relay.example.com    # HTTPS with a free Let's Encrypt certificate
```

If C can't reach GitHub, download the file elsewhere (or build it with
`./build.sh` → `dist/filerelay-install.sh`) and copy it over with
`scp filerelay-install.sh root@C:`. Checksums are in each release's `SHA256SUMS`.

It installs `/usr/local/bin/filerelay`, creates a `filerelay` system user and
`/var/lib/filerelay`, writes `/etc/filerelay.env` with fresh tokens, sets up and
starts a hardened systemd service, opens the port in ufw/firewalld if one is
active, checks that the service answers, and prints the URL, the tokens and
the exact commands to run on A and B.

| Command | Effect |
|---|---|
| `sudo bash filerelay-install.sh` again | Upgrade in place; tokens and settings are kept |
| `... --port 9000 --ttl 168h --max-upload 50G` | Change settings (also on upgrade) |
| `... --domain NAME [--email ADDR]` | HTTPS on 443 (+80 for certificate challenges and redirects). DNS for NAME must point at C. |
| `... --public-url URL` | URL to print/advertise when A and B reach C by another address (NAT, proxy) |
| `... --reset-tokens` / `--no-read-token` | New tokens / a single read-write token only |
| `... --status` | Show service state, URL, tokens and client commands again |
| `... --uninstall [--purge]` | Remove the service (`--purge` also deletes data, config and the user) |

Rebuild the installer after code changes with `./build.sh`.

### Option 2: Docker

```bash
echo "RELAY_TOKEN=$(openssl rand -base64 24 | tr '+/' '-_')" > .env
docker compose up -d --build
```

### HTTPS

Tokens travel in every request, so use HTTPS whenever traffic crosses the
internet. Either:

- use `--domain` (installer) / `-domain` (binary): built-in automatic
  Let's Encrypt certificates, renewed automatically;
- put Caddy or nginx in front (`deploy/Caddyfile`), keeping filerelay on `127.0.0.1:8080`;
- or run filerelay with `-tls-cert` / `-tls-key`.

Without a domain, an SSH tunnel also works:
`ssh -N -L 8080:127.0.0.1:8080 C` on A and on B, then use `http://127.0.0.1:8080`.

### Configuration

Every flag has an environment variable.

| Flag | Env | Default | Meaning |
|---|---|---|---|
| `-listen` | `RELAY_LISTEN` | `:8080` | Listen address |
| `-data` | `RELAY_DATA` | `./data` | Data directory |
| `-token` | `RELAY_TOKEN` | generated → `data/token` | Read-write token |
| `-read-token` | `RELAY_READ_TOKEN` | none | Optional read-only token |
| `-ttl` | `RELAY_TTL` | `72h` | Expiry after upload (`0` = keep forever) |
| `-max-upload` | `RELAY_MAX_UPLOAD` | `0` (no limit) | Max size per file, e.g. `50G` |
| `-public-url` | `RELAY_PUBLIC_URL` | from request | URL shown in the UI and `/agent.md` |
| `-domain` | `RELAY_DOMAIN` | none | Automatic HTTPS (Let's Encrypt) for this domain; listens on :443 and :80 |
| `-acme-email` | `RELAY_ACME_EMAIL` | none | Contact address for Let's Encrypt |
| `-http-listen` | `RELAY_HTTP_LISTEN` | `:80` | With `-domain`: address for challenges and the HTTP→HTTPS redirect |
| `-tls-cert`, `-tls-key` | `RELAY_TLS_CERT`, `RELAY_TLS_KEY` | none | Serve HTTPS with your own certificate |

## Migrating A → B

On **both** A and B:

```bash
curl -fsSL https://relay.example.com/relay.sh -o relay && chmod +x relay
export RELAY_URL=https://relay.example.com
export RELAY_TOKEN=...            # read-write on A; the read-only token is enough on B
```

On **A**:

```bash
./relay pack /srv/myapp /migration          # tar.gz (keeps perms/symlinks) + upload
pg_dump mydb > db.sql && ./relay push db.sql /migration
./relay ls /migration
```

On **B**:

```bash
./relay unpack /migration/myapp.tar.gz /srv # download + verify + extract
./relay pull /migration/db.sql .            # download + verify
```

Clean up afterwards with `./relay rm /migration` (read-write token), or let
the TTL handle it.

Re-running any command is safe: finished files are skipped and partial ones
resume. `RELAY_RETRIES` (default 30) controls persistence; `RELAY_CURL_OPTS`
passes extra curl options (e.g. `-k` for a self-signed certificate,
`--limit-rate 20M`).

### Prompt for the AI agent

> Migrate `/srv/myapp` and the `mydb` PostgreSQL database from server A to
> server B. A and B can't reach each other reliably, so relay everything
> through `https://relay.example.com`. First run
> `curl -fsSL https://relay.example.com/agent.md` to learn how to use it.
> The token is in `$RELAY_TOKEN` on both servers. Verify that every transfer
> completed with a matching checksum before you continue, and delete
> `/migration` from the relay when B is confirmed working.

### With plain curl

```bash
H="Authorization: Bearer $RELAY_TOKEN"
# A: upload with verification
curl -fsS -H "$H" -H "X-Checksum-Sha256: $(sha256sum f.tgz | cut -d' ' -f1)" -T f.tgz $RELAY_URL/api/files/migration/f.tgz
# A: resume an interrupted upload
off=$(curl -fsS -H "$H" "$RELAY_URL/api/upload-status/migration/f.tgz?format=text")
curl -fsS -H "$H" -C "$off" -T f.tgz $RELAY_URL/api/files/migration/f.tgz
# B: download (re-run to resume), then verify
curl -fsS -H "$H" -C - -o f.tgz $RELAY_URL/api/files/migration/f.tgz
curl -fsS -H "$H" "$RELAY_URL/api/sum/migration/f.tgz?format=text" | sha256sum -c
```

The full API is listed at `/agent.md` on a running relay.

### With WebDAV / rclone

```bash
rclone config create relay webdav url=https://relay.example.com/dav/ vendor=other \
  user=x pass=$(rclone obscure "$RELAY_TOKEN")
rclone copy /srv/myapp relay:migration/myapp --progress   # on A
rclone copy relay:migration/myapp /srv/myapp --progress   # on B
```

WebDAV writes files in place, so a reader can see a file while it is still
uploading. Prefer the API or the `relay` script when B pulls while A is still
pushing.

## API summary

| Method | Path | Access |
|---|---|---|
| `PUT` | `/api/files/<path>` (optional `Content-Range`, `X-Checksum-Sha256`, `X-Total-Size`) | write |
| `GET`/`HEAD` | `/api/files/<path>` (Range supported, `?download=1`) | read |
| `DELETE` | `/api/files/<path>` | write |
| `GET` | `/api/upload-status/<path>` | write |
| `POST` | `/api/mkdir/<path>` | write |
| `GET` | `/api/ls/<dir>`, `/api/sum/<path>`, `/api/manifest/<path>?sha256=1` | read |
| `GET` | `/api/archive/<dir>` (tar.gz; `?gzip=0` for tar) | read |
| `GET` | `/api/info` | read |
| `POST` | `/api/login`, `/api/logout` (UI session) | — |
| any | `/dav/...` (WebDAV, Basic auth) | read / write |
| `GET` | `/`, `/agent.md`, `/relay.sh`, `/healthz` | public |

`ls`, `sum`, `manifest` and `upload-status` accept `?format=text` for
shell-friendly output.

## Development

```bash
go test ./...
go run . -data ./data -token dev
./build.sh        # dist/: binaries for linux amd64/arm64/arm, macOS arm64, windows amd64,
                  # plus the one-click installer dist/filerelay-install.sh (from deploy/install.sh)
```

Source: `main.go` (config), `server.go` (routing, auth, paths, expiry),
`api.go` (REST handlers), `dav.go` (WebDAV), `agentdoc.go` (`/agent.md`),
`web/index.html` (UI), `client/relay.sh` (client). The UI and the client
script are embedded into the binary.
