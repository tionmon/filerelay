package main

import (
	"net/http"
	"strings"
)

// agentDoc is served at /agent.md so an AI agent (or a human) on either end can
// learn how to use this relay with one request. {{BASE}} is replaced with the
// relay's external URL.
const agentDoc = `# File relay: usage for agents

This server is a file relay. The source host pushes files here; the
destination host pulls them. Every transfer can resume after a dropped
connection and is verified with SHA-256.

Base URL: {{BASE}}
Auth: send ` + "`Authorization: Bearer $RELAY_TOKEN`" + ` (or HTTP Basic auth with any
username and the token as password). Ask the operator for the token; it is
never shown on this page.

## Recommended: the relay client script (bash + curl)

    curl -fsSL {{BASE}}/relay.sh -o relay && chmod +x relay
    export RELAY_URL={{BASE}} RELAY_TOKEN=<token>

On the SOURCE host:

    ./relay pack /srv/myapp /migration      # tar.gz the dir (keeps perms, symlinks), upload with resume
    ./relay push ./dump.sql /migration      # upload one file
    ./relay push ./assets /migration        # upload a directory file by file

On the DESTINATION host:

    ./relay unpack /migration/myapp.tar.gz /srv   # download with resume, verify, extract into /srv
    ./relay pull /migration/dump.sql .            # download one file, verify
    ./relay pull /migration/assets .              # download a directory, verify every file

Other commands: ` + "`./relay ls [dir]`, `./relay sum <path>`, `./relay rm <path>`, `./relay info`" + `.
The script retries automatically (RELAY_RETRIES, default 30) and exits non-zero
only if a transfer could not be completed and verified. Re-running a command is
safe: finished files are skipped, partial ones resume.

## Raw HTTP API

Paths below are URL-encoded relay paths, e.g. /migration/app.tar.gz.

| Method | Endpoint | Purpose |
|---|---|---|
| PUT | /api/files/<path> | Upload. Body = file bytes. Optional header ` + "`X-Checksum-Sha256`" + ` is verified; mismatch → 422 and the upload is discarded. Response JSON has size and sha256. |
| GET | /api/upload-status/<path>?format=text | Bytes already received for an interrupted upload. |
| PUT | /api/files/<path> + ` + "`Content-Range: bytes N-(size-1)/size`" + ` | Resume an upload from byte N. ` + "`curl -C N -T file URL`" + ` sends this header. |
| GET | /api/files/<path> | Download. Supports Range, so ` + "`curl -C - -o file URL`" + ` resumes. |
| GET | /api/sum/<path>?format=text | SHA-256 in sha256sum format. |
| GET | /api/ls/<dir>?format=text | List a directory: type, size, mtime, name (tab-separated). |
| GET | /api/manifest/<path>?sha256=1&format=tsv | All files under a path: size, sha256, relative path (tab-separated). |
| GET | /api/archive/<dir> | Stream a directory as tar.gz (not resumable). |
| POST | /api/mkdir/<dir> | Create a directory. |
| DELETE | /api/files/<path> | Delete a file or directory. |
| GET | /api/info | Server info: TTL, usage, your access level. |

Uploads are atomic: a file becomes visible to readers only after the last byte
has arrived (and the checksum matched, if given).

Example with curl only:

    sum=$(sha256sum app.tar.gz | cut -d' ' -f1)
    curl -fsS -H "Authorization: Bearer $RELAY_TOKEN" -H "X-Checksum-Sha256: $sum" \
         -T app.tar.gz {{BASE}}/api/files/migration/app.tar.gz
    # on the other side:
    curl -fsS -C - -H "Authorization: Bearer $RELAY_TOKEN" -o app.tar.gz \
         {{BASE}}/api/files/migration/app.tar.gz
    curl -fsS -H "Authorization: Bearer $RELAY_TOKEN" "{{BASE}}/api/sum/migration/app.tar.gz?format=text" | sha256sum -c

## WebDAV

Mount or sync {{BASE}}/dav/ with any WebDAV client (rclone, davfs2, Finder,
Windows Explorer). Username: anything. Password: the token.
WebDAV uploads are written in place, so readers can see a partial file while it
uploads; use the API or relay script when atomicity matters.

## Expiry

{{EXPIRY}}
`

func (s *Server) handleAgentDoc(w http.ResponseWriter, r *http.Request) {
	expiry := "Files are kept until deleted."
	if s.cfg.TTL > 0 {
		expiry = "Files are deleted automatically " + s.cfg.TTL.String() + " after their upload completed. Delete them yourself once the migration is verified."
	}
	doc := strings.NewReplacer("{{BASE}}", s.baseURL(r), "{{EXPIRY}}", expiry).Replace(agentDoc)
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Write([]byte(doc))
}
