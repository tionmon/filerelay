// filerelay is a small file relay station: one host pushes files in, another
// pulls them out. It exposes a REST API (resumable, checksummed), WebDAV and a
// web UI from a single binary.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/acme/autocert"
)

var version = "0.1.1"

type Config struct {
	Listen    string
	DataDir   string
	Token     string
	ReadToken string
	TTL       time.Duration
	MaxUpload int64
	TLSCert   string
	TLSKey    string
	PublicURL string
	// Automatic HTTPS via Let's Encrypt.
	Domains    []string
	ACMEEmail  string
	HTTPListen string
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// parseSize accepts plain bytes or a K/M/G/T suffix (binary units).
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	if s == "" {
		return 0, nil
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return int64(n * float64(mult)), nil
}

// loadOrCreateToken keeps a generated token stable across restarts.
func loadOrCreateToken(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "token")
	if b, err := os.ReadFile(p); err == nil {
		if t := strings.TrimSpace(string(b)); t != "" {
			return t, nil
		}
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	t := base64.RawURLEncoding.EncodeToString(buf)
	if err := os.WriteFile(p, []byte(t+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Printf("generated access token (stored in %s): %s", p, t)
	return t, nil
}

func main() {
	var cfg Config
	var ttl, maxUpload, domains string
	var showVersion bool
	flag.StringVar(&cfg.Listen, "listen", envOr("RELAY_LISTEN", ":8080"), "listen address (env RELAY_LISTEN)")
	flag.StringVar(&cfg.DataDir, "data", envOr("RELAY_DATA", "./data"), "data directory (env RELAY_DATA)")
	flag.StringVar(&cfg.Token, "token", envOr("RELAY_TOKEN", ""), "read-write access token; generated and stored in <data>/token if empty (env RELAY_TOKEN)")
	flag.StringVar(&cfg.ReadToken, "read-token", envOr("RELAY_READ_TOKEN", ""), "optional read-only token (env RELAY_READ_TOKEN)")
	flag.StringVar(&ttl, "ttl", envOr("RELAY_TTL", "72h"), "delete files this long after upload; 0 disables (env RELAY_TTL)")
	flag.StringVar(&maxUpload, "max-upload", envOr("RELAY_MAX_UPLOAD", "0"), "max size of one file, e.g. 50G; 0 = unlimited (env RELAY_MAX_UPLOAD)")
	flag.StringVar(&cfg.TLSCert, "tls-cert", envOr("RELAY_TLS_CERT", ""), "TLS certificate file (env RELAY_TLS_CERT)")
	flag.StringVar(&cfg.TLSKey, "tls-key", envOr("RELAY_TLS_KEY", ""), "TLS key file (env RELAY_TLS_KEY)")
	flag.StringVar(&cfg.PublicURL, "public-url", envOr("RELAY_PUBLIC_URL", ""), "external base URL used in generated instructions, e.g. https://relay.example.com (env RELAY_PUBLIC_URL)")
	flag.StringVar(&domains, "domain", envOr("RELAY_DOMAIN", ""), "serve HTTPS for this domain (comma-separated for several) with an automatic Let's Encrypt certificate; listens on :443 and :80 (env RELAY_DOMAIN)")
	flag.StringVar(&cfg.ACMEEmail, "acme-email", envOr("RELAY_ACME_EMAIL", ""), "optional contact email for Let's Encrypt (env RELAY_ACME_EMAIL)")
	flag.StringVar(&cfg.HTTPListen, "http-listen", envOr("RELAY_HTTP_LISTEN", ":80"), "with -domain: plain-HTTP address for certificate challenges and redirects (env RELAY_HTTP_LISTEN)")
	flag.BoolVar(&showVersion, "version", false, "print version and exit")
	flag.Parse()

	if showVersion {
		fmt.Println("filerelay", version)
		return
	}

	var err error
	if cfg.TTL, err = time.ParseDuration(ttl); err != nil {
		log.Fatalf("invalid -ttl: %v", err)
	}
	if cfg.MaxUpload, err = parseSize(maxUpload); err != nil {
		log.Fatalf("invalid -max-upload: %v", err)
	}
	cfg.PublicURL = strings.TrimRight(cfg.PublicURL, "/")
	for _, d := range strings.Split(domains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			cfg.Domains = append(cfg.Domains, d)
		}
	}
	if len(cfg.Domains) > 0 {
		listenSet := os.Getenv("RELAY_LISTEN") != ""
		flag.Visit(func(f *flag.Flag) { listenSet = listenSet || f.Name == "listen" })
		if !listenSet {
			cfg.Listen = ":443"
		}
		if cfg.PublicURL == "" {
			cfg.PublicURL = "https://" + cfg.Domains[0]
		}
		if cfg.TLSCert != "" {
			log.Fatal("-domain and -tls-cert are mutually exclusive")
		}
	}

	if cfg.DataDir, err = filepath.Abs(cfg.DataDir); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		log.Fatal(err)
	}
	if cfg.Token == "" {
		if cfg.Token, err = loadOrCreateToken(cfg.DataDir); err != nil {
			log.Fatalf("token: %v", err)
		}
	}
	if cfg.ReadToken == cfg.Token {
		log.Fatal("-read-token must differ from -token")
	}

	s, err := NewServer(cfg)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go s.janitor(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No Read/WriteTimeout: transfers of large files may take hours.
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx)
	}()

	log.Printf("filerelay %s listening on %s, data in %s, ttl %v", version, cfg.Listen, cfg.DataDir, cfg.TTL)
	switch {
	case len(cfg.Domains) > 0:
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.Domains...),
			Cache:      autocert.DirCache(filepath.Join(cfg.DataDir, "acme")),
			Email:      cfg.ACMEEmail,
		}
		srv.TLSConfig = m.TLSConfig()
		// Port 80 answers HTTP-01 challenges and redirects everything else to HTTPS.
		// If it can't bind, TLS-ALPN-01 on :443 still obtains certificates.
		httpSrv := &http.Server{Addr: cfg.HTTPListen, Handler: m.HTTPHandler(nil), ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("http listener on %s: %v (continuing with HTTPS only)", cfg.HTTPListen, err)
			}
		}()
		go func() { <-ctx.Done(); httpSrv.Close() }()
		log.Printf("automatic HTTPS for %s (certificates cached in %s)", strings.Join(cfg.Domains, ", "), filepath.Join(cfg.DataDir, "acme"))
		err = srv.ListenAndServeTLS("", "")
	case cfg.TLSCert != "":
		err = srv.ListenAndServeTLS(cfg.TLSCert, cfg.TLSKey)
	default:
		err = srv.ListenAndServe()
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
