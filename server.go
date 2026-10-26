package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

const (
	sessionTTL    = 30 * 24 * time.Hour
	maxFailures   = 5
	failureWindow = 10 * time.Minute
)

type server struct {
	store      *Store
	password   string
	pullToken  string
	key        []byte // cookie signing key
	trustProxy bool

	mu       sync.Mutex
	failures map[string][]time.Time
}

func runServe(ctx context.Context) error {
	password, err := requireSecret("RELAYPORT_UPLOAD_PASSWORD")
	if err != nil {
		return err
	}
	pullToken, err := requireSecret("RELAYPORT_PULL_TOKEN")
	if err != nil {
		return err
	}
	if len(pullToken) < 16 {
		log.Print("warning: RELAYPORT_PULL_TOKEN is short; use a long random value (openssl rand -hex 32)")
	}
	quota, err := parseSize(envOr("RELAYPORT_QUOTA", "0"))
	if err != nil {
		return fmt.Errorf("RELAYPORT_QUOTA: %w", err)
	}
	ttl, err := time.ParseDuration(envOr("RELAYPORT_STALE_TTL", "48h"))
	if err != nil {
		return fmt.Errorf("RELAYPORT_STALE_TTL: %w", err)
	}
	store, err := NewStore(envOr("RELAYPORT_DATA", "./data"), quota)
	if err != nil {
		return err
	}
	secret := envOr("RELAYPORT_SECRET", password+"/relayport/session")
	s := &server{
		store:      store,
		password:   password,
		pullToken:  pullToken,
		key:        []byte(secret),
		trustProxy: os.Getenv("RELAYPORT_TRUST_PROXY") == "1",
		failures:   map[string][]time.Time{},
	}

	go func() {
		for t := time.NewTicker(time.Hour); ; {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				store.Sweep(ttl)
			}
		}
	}()

	srv := &http.Server{
		Addr:              envOr("RELAYPORT_LISTEN", ":8080"),
		Handler:           s.routes(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sh)
	}()
	log.Printf("serving on %s, data in %s, quota %s", srv.Addr, envOr("RELAYPORT_DATA", "./data"), envOr("RELAYPORT_QUOTA", "0"))
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(indexHTML)
	})
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("GET /api/state", s.session(s.state))
	mux.HandleFunc("POST /api/uploads", s.session(s.createUpload))
	mux.HandleFunc("PUT /api/uploads/{id}/chunks/{n}", s.session(s.putChunk))
	mux.HandleFunc("POST /api/uploads/{id}/complete", s.session(s.complete))

	mux.HandleFunc("GET /pull/list", s.token(s.pullList))
	mux.HandleFunc("GET /pull/file", s.token(s.pullFile))
	mux.HandleFunc("POST /pull/progress", s.token(s.pullProgress))
	mux.HandleFunc("POST /pull/done", s.token(s.pullDone))
	return mux
}

// --- auth ---

// clientIP is the address rate limits are keyed on. With trustProxy,
// X-Forwarded-For is only believed when the connection comes from a loopback
// or private address (the reverse proxy, or Docker's gateway in front of it),
// so a client reaching an exposed port directly cannot spoof its address. The
// last entry is the one added by the nearest proxy; earlier ones are client-supplied.
func (s *server) clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		h = r.RemoteAddr
	}
	if s.trustProxy {
		if peer := net.ParseIP(h); peer != nil && (peer.IsLoopback() || peer.IsPrivate()) {
			if v := r.Header.Get("X-Forwarded-For"); v != "" {
				parts := strings.Split(v, ",")
				return strings.TrimSpace(parts[len(parts)-1])
			}
		}
	}
	return h
}

// limited and fail count failures per key ("login <ip>" or "pull <ip>"), so a
// puller with a wrong token does not lock the web page out of the same address.
func (s *server) limited(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	recent := s.failures[key][:0]
	for _, t := range s.failures[key] {
		if time.Since(t) < failureWindow {
			recent = append(recent, t)
		}
	}
	s.failures[key] = recent
	return len(recent) >= maxFailures
}

func (s *server) fail(key string) {
	s.mu.Lock()
	s.failures[key] = append(s.failures[key], time.Now())
	s.mu.Unlock()
}

func eq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

func (s *server) sign(exp int64) string {
	msg := strconv.FormatInt(exp, 10)
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte(msg))
	return msg + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (s *server) validCookie(v string) bool {
	i := strings.IndexByte(v, '.')
	if i < 0 {
		return false
	}
	exp, err := strconv.ParseInt(v[:i], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return eq(v, s.sign(exp))
}

func (s *server) login(w http.ResponseWriter, r *http.Request) {
	key := "login " + s.clientIP(r)
	if s.limited(key) {
		http.Error(w, "too many failed logins from this address, try again in 10 minutes", http.StatusTooManyRequests)
		return
	}
	var in struct{ Password string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in) != nil || !eq(in.Password, s.password) {
		s.fail(key)
		http.Error(w, "wrong password", http.StatusUnauthorized)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: "relayport_session", Value: s.sign(time.Now().Add(sessionTTL).Unix()),
		Path: "/", MaxAge: int(sessionTTL.Seconds()), HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
	})
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) session(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("relayport_session")
		if err != nil || !s.validCookie(c.Value) {
			http.Error(w, "login required", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func (s *server) token(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := "pull " + s.clientIP(r)
		if s.limited(key) {
			http.Error(w, "too many wrong pull tokens from this address, try again in 10 minutes", http.StatusTooManyRequests)
			return
		}
		if !eq(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), s.pullToken) {
			s.fail(key)
			http.Error(w, "pull token rejected", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// --- upload API ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func storeError(w http.ResponseWriter, err error) {
	code := http.StatusInternalServerError
	switch {
	case errors.Is(err, errBadPath):
		code = http.StatusBadRequest
	case errors.Is(err, errNotFound):
		code = http.StatusNotFound
	case errors.Is(err, errExists):
		code = http.StatusConflict
	case errors.Is(err, errQuota):
		code = http.StatusInsufficientStorage
	}
	http.Error(w, err.Error(), code)
}

func (s *server) state(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.store.Snapshot()) }

func (s *server) createUpload(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Key, Name string
		Size      int64
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil || in.Key == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u, err := s.store.Create(in.Key, in.Name, in.Size)
	if err != nil {
		storeError(w, err)
		return
	}
	s.store.mu.Lock()
	out := map[string]any{"id": u.ID, "chunkSize": u.ChunkSize, "done": append([]bool(nil), u.Done...)}
	s.store.mu.Unlock()
	writeJSON(w, out)
}

func (s *server) putChunk(w http.ResponseWriter, r *http.Request) {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		http.Error(w, "bad chunk number", http.StatusBadRequest)
		return
	}
	if err := s.store.WriteChunk(r.PathValue("id"), n, r.Body, r.Header.Get("X-Chunk-Sha256")); err != nil {
		if errors.Is(err, errNotFound) {
			storeError(w, err)
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) complete(w http.ResponseWriter, r *http.Request) {
	st, err := s.store.Complete(r.PathValue("id"))
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, st)
}

// --- pull API ---

func (s *server) pullList(w http.ResponseWriter, r *http.Request) {
	files := s.store.Ready()
	if files == nil {
		files = []Staged{}
	}
	writeJSON(w, files)
}

func (s *server) pullFile(w http.ResponseWriter, r *http.Request) {
	f, st, err := s.store.Open(r.URL.Query().Get("name"))
	if err != nil {
		storeError(w, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, f)
	_ = st
}

func (s *server) pullProgress(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name  string
		Bytes int64
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	s.store.Progress(in.Name, in.Bytes)
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) pullDone(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name, SHA256 string }
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.store.Finish(in.Name, in.SHA256); err != nil {
		if errors.Is(err, errNotFound) {
			storeError(w, err)
			return
		}
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseSize understands plain bytes and KiB/MiB/GiB/TiB suffixes (also KB, MB, ... as binary).
func parseSize(v string) (int64, error) {
	v = strings.TrimSpace(strings.ToUpper(v))
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"TIB", 1 << 40}, {"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10}, {"TB", 1 << 40}, {"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1}} {
		if strings.HasSuffix(v, u.suf) {
			v, mult = strings.TrimSpace(strings.TrimSuffix(v, u.suf)), u.m
			break
		}
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", v)
	}
	return int64(n * float64(mult)), nil
}
