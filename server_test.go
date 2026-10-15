package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func init() { log.SetOutput(io.Discard) }

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	s, err := NewServer(Config{DataDir: t.TempDir(), Token: "rw", ReadToken: "ro", TTL: time.Hour, MaxUpload: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func do(t *testing.T, method, url, token string, body io.Reader, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, url, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, b
}

func sum(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

func TestAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	if res, _ := do(t, "GET", ts.URL+"/api/ls/", "", nil, nil); res.StatusCode != 401 {
		t.Fatalf("no token: got %d", res.StatusCode)
	}
	if res, _ := do(t, "GET", ts.URL+"/api/ls/", "wrong", nil, nil); res.StatusCode != 401 {
		t.Fatalf("bad token: got %d", res.StatusCode)
	}
	if res, _ := do(t, "PUT", ts.URL+"/api/files/a", "ro", strings.NewReader("x"), nil); res.StatusCode != 403 {
		t.Fatalf("read-only put: got %d", res.StatusCode)
	}
	if res, _ := do(t, "PUT", ts.URL+"/dav/a", "ro", strings.NewReader("x"), nil); res.StatusCode != 403 {
		t.Fatalf("read-only dav put: got %d", res.StatusCode)
	}
	if res, _ := do(t, "GET", ts.URL+"/api/ls/", "ro", nil, nil); res.StatusCode != 200 {
		t.Fatalf("read-only ls: got %d", res.StatusCode)
	}
}

func TestChunkedResumeAndChecksum(t *testing.T) {
	ts, _ := newTestServer(t)
	data := bytes.Repeat([]byte("0123456789"), 5000) // 50 KB
	url := ts.URL + "/api/files/dir/f%20one.bin"

	res, _ := do(t, "PUT", url, "rw", bytes.NewReader(data[:20000]), map[string]string{"Content-Range": fmt.Sprintf("bytes 0-19999/%d", len(data))})
	if res.StatusCode != 202 {
		t.Fatalf("first chunk: got %d", res.StatusCode)
	}
	if res, _ := do(t, "GET", url, "ro", nil, nil); res.StatusCode != 404 {
		t.Fatalf("partial upload must not be visible, got %d", res.StatusCode)
	}
	_, b := do(t, "GET", ts.URL+"/api/upload-status/dir/f%20one.bin?format=text", "rw", nil, nil)
	if strings.TrimSpace(string(b)) != "20000" {
		t.Fatalf("upload-status = %q", b)
	}
	// Offset beyond what the server has is rejected.
	if res, _ := do(t, "PUT", url, "rw", bytes.NewReader(data[30000:]), map[string]string{"Content-Range": fmt.Sprintf("bytes 30000-%d/%d", len(data)-1, len(data))}); res.StatusCode != 409 {
		t.Fatalf("gap: got %d", res.StatusCode)
	}
	res, b = do(t, "PUT", url, "rw", bytes.NewReader(data[20000:]), map[string]string{
		"Content-Range":     fmt.Sprintf("bytes 20000-%d/%d", len(data)-1, len(data)),
		"X-Checksum-Sha256": sum(data),
	})
	if res.StatusCode != 201 {
		t.Fatalf("final chunk: got %d %s", res.StatusCode, b)
	}
	var out struct{ SHA256 string }
	json.Unmarshal(b, &out)
	if out.SHA256 != sum(data) {
		t.Fatalf("sha mismatch")
	}
	// Ranged download continues from an offset.
	res, b = do(t, "GET", url, "ro", nil, map[string]string{"Range": "bytes=40000-"})
	if res.StatusCode != 206 || !bytes.Equal(b, data[40000:]) {
		t.Fatalf("range: %d len %d", res.StatusCode, len(b))
	}
	// Wrong checksum discards the upload.
	res, _ = do(t, "PUT", ts.URL+"/api/files/bad", "rw", strings.NewReader("abc"), map[string]string{"X-Checksum-Sha256": "00"})
	if res.StatusCode != 422 {
		t.Fatalf("bad checksum: got %d", res.StatusCode)
	}
	if res, _ := do(t, "GET", ts.URL+"/api/files/bad", "rw", nil, nil); res.StatusCode != 404 {
		t.Fatalf("rejected upload must not exist")
	}
}

func TestLimitsAndPaths(t *testing.T) {
	ts, s := newTestServer(t)
	if res, _ := do(t, "PUT", ts.URL+"/api/files/big", "rw", bytes.NewReader(make([]byte, 2<<20)), nil); res.StatusCode != 413 {
		t.Fatalf("too large: got %d", res.StatusCode)
	}
	do(t, "PUT", ts.URL+"/api/files/..%2F..%2Fescape", "rw", strings.NewReader("x"), nil)
	if _, err := os.Stat(filepath.Join(s.cfg.DataDir, "..", "escape")); err == nil {
		t.Fatal("path traversal escaped the data dir")
	}
	if _, err := os.Stat(filepath.Join(s.root, "escape")); err != nil {
		t.Fatal("traversal path should be confined to the root")
	}
	if res, _ := do(t, "DELETE", ts.URL+"/api/files/", "rw", nil, nil); res.StatusCode != 400 {
		t.Fatalf("delete root: got %d", res.StatusCode)
	}
}

func TestManifestAndSweep(t *testing.T) {
	ts, s := newTestServer(t)
	for _, p := range []string{"m/a.txt", "m/sub/b.txt"} {
		do(t, "PUT", ts.URL+"/api/files/"+p, "rw", strings.NewReader(p), nil)
	}
	_, b := do(t, "GET", ts.URL+"/api/manifest/m?sha256=1&format=tsv", "ro", nil, nil)
	want := fmt.Sprintf("7\t%s\tm/a.txt\n11\t%s\tm/sub/b.txt\n", sum([]byte("m/a.txt")), sum([]byte("m/sub/b.txt")))
	if string(b) != want {
		t.Fatalf("manifest:\n%s\nwant:\n%s", b, want)
	}
	old := time.Now().Add(-2 * time.Hour)
	os.Chtimes(filepath.Join(s.root, "m", "sub", "b.txt"), old, old)
	s.sweep()
	if _, err := os.Stat(filepath.Join(s.root, "m", "sub")); err == nil {
		t.Fatal("expired file's empty dir should be gone")
	}
	if _, err := os.Stat(filepath.Join(s.root, "m", "a.txt")); err != nil {
		t.Fatal("fresh file must survive the sweep")
	}
}
