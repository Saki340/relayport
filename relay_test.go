package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestServer(t *testing.T, quota int64) (*server, *httptest.Server) {
	t.Helper()
	store, err := NewStore(t.TempDir(), quota)
	if err != nil {
		t.Fatal(err)
	}
	s := &server{store: store, password: "pw", pullToken: "tok", key: []byte("k"), failures: map[string][]time.Time{}}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return s, ts
}

func login(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	jar := newJar()
	c := &http.Client{Jar: jar}
	resp, err := c.Post(ts.URL+"/api/login", "application/json", bytes.NewReader([]byte(`{"Password":"pw"}`)))
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("login: %v %v", err, resp)
	}
	return c
}

func uploadFile(t *testing.T, c *http.Client, ts *httptest.Server, name string, data []byte, skip map[int]bool) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"Key": name + "k", "Name": name, "Size": len(data)})
	resp, err := c.Post(ts.URL+"/api/uploads", "application/json", bytes.NewReader(body))
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("create: %v %v", err, resp)
	}
	var u struct {
		ID        string
		ChunkSize int64
		Done      []bool
	}
	json.NewDecoder(resp.Body).Decode(&u)
	for i, done := range u.Done {
		if done || skip[i] {
			continue
		}
		lo, hi := int64(i)*u.ChunkSize, int64(i+1)*u.ChunkSize
		if hi > int64(len(data)) {
			hi = int64(len(data))
		}
		sum := sha256.Sum256(data[lo:hi])
		req, _ := http.NewRequest("PUT", fmt.Sprintf("%s/api/uploads/%s/chunks/%d", ts.URL, u.ID, i), bytes.NewReader(data[lo:hi]))
		req.Header.Set("X-Chunk-Sha256", hex.EncodeToString(sum[:]))
		r, err := c.Do(req)
		if err != nil || r.StatusCode != 204 {
			t.Fatalf("chunk %d: %v %v", i, err, r)
		}
	}
}

func complete(t *testing.T, c *http.Client, ts *httptest.Server, name string) int {
	t.Helper()
	id := uploadID(name + "k")
	r, err := c.Post(fmt.Sprintf("%s/api/uploads/%s/complete", ts.URL, id), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	return r.StatusCode
}

func TestEndToEnd(t *testing.T) {
	s, ts := newTestServer(t, 0)
	c := login(t, ts)

	data := make([]byte, 20<<20+123) // 3 chunks of 8 MiB, last one short
	rand.Read(data)
	uploadFile(t, c, ts, "dir/big.bin", data, map[int]bool{1: true}) // chunk 1 "lost"
	if code := complete(t, c, ts, "dir/big.bin"); code == 200 {
		t.Fatal("complete must fail while a chunk is missing")
	}
	uploadFile(t, c, ts, "dir/big.bin", data, nil) // resume sends only chunk 1
	if code := complete(t, c, ts, "dir/big.bin"); code != 200 {
		t.Fatalf("complete: %d", code)
	}
	empty := []byte{}
	uploadFile(t, c, ts, "empty.txt", empty, nil)
	if code := complete(t, c, ts, "empty.txt"); code != 200 {
		t.Fatalf("complete empty: %d", code)
	}

	deadline := time.Now().Add(10 * time.Second)
	for len(s.store.Ready()) < 2 {
		if time.Now().After(deadline) {
			t.Fatal("files never became ready")
		}
		time.Sleep(50 * time.Millisecond)
	}

	dest := t.TempDir()
	p := &puller{server: ts.URL, token: "tok", dest: dest, segSize: 4 << 20,
		slots: make(chan struct{}, 3), client: http.DefaultClient, active: map[string]bool{}}
	list, err := p.list(context.Background())
	if err != nil || len(list) != 2 {
		t.Fatalf("list: %v %v", list, err)
	}
	for _, f := range list {
		if err := p.pullOne(context.Background(), f); err != nil {
			t.Fatalf("pull %s: %v", f.Name, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dest, "dir", "big.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("pulled content differs: %v", err)
	}
	if left := s.store.Snapshot().Staged; len(left) != 0 {
		t.Fatalf("relay still holds %v", left)
	}
	if ents, _ := os.ReadDir(filepath.Join(dest, "dir")); len(ents) != 1 {
		t.Fatalf("leftover files in dest: %v", ents)
	}
	if _, err := os.Stat(filepath.Join(s.store.inboxDir(), "dir")); err == nil {
		t.Fatal("empty directory left on relay")
	}
}

func TestPullResumesAndRejectsCorruptState(t *testing.T) {
	s, ts := newTestServer(t, 0)
	c := login(t, ts)
	data := make([]byte, 9<<20)
	rand.Read(data)
	uploadFile(t, c, ts, "a.bin", data, nil)
	complete(t, c, ts, "a.bin")
	for len(s.store.Ready()) < 1 {
		time.Sleep(20 * time.Millisecond)
	}
	dest := t.TempDir()
	p := &puller{server: ts.URL, token: "tok", dest: dest, segSize: 2 << 20,
		slots: make(chan struct{}, 2), client: http.DefaultClient, active: map[string]bool{}}
	f := s.store.Ready()[0]

	// A stale state file claiming every segment is done, but with garbage content,
	// must be caught by the final checksum and recovered from.
	st := pullState{Size: f.Size, SHA256: f.SHA256, SegSize: p.segSize, Done: make([]bool, 5)}
	for i := range st.Done {
		st.Done[i] = true
	}
	b, _ := json.Marshal(st)
	os.WriteFile(filepath.Join(dest, ".a.bin.state"), b, 0o644)
	os.WriteFile(filepath.Join(dest, ".a.bin.part"), make([]byte, f.Size), 0o644)
	if err := p.pullOne(context.Background(), f); err == nil {
		t.Fatal("expected checksum mismatch")
	}
	if err := p.pullOne(context.Background(), f); err != nil {
		t.Fatalf("second attempt should recover: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "a.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("content differs after recovery")
	}
}

func TestPullNeverOverwritesLocalFile(t *testing.T) {
	s, ts := newTestServer(t, 0)
	c := login(t, ts)
	data := make([]byte, 3<<20)
	rand.Read(data)
	uploadFile(t, c, ts, "a.bin", data, nil)
	complete(t, c, ts, "a.bin")
	for len(s.store.Ready()) < 1 {
		time.Sleep(20 * time.Millisecond)
	}
	f := s.store.Ready()[0]

	// Same name, different size, and same size with different content.
	for _, local := range [][]byte{[]byte("someone else's file"), make([]byte, len(data))} {
		dest := t.TempDir()
		p := &puller{server: ts.URL, token: "tok", dest: dest, segSize: 1 << 20,
			slots: make(chan struct{}, 2), client: http.DefaultClient, active: map[string]bool{}}
		os.WriteFile(filepath.Join(dest, "a.bin"), local, 0o644)
		if err := p.pullOne(context.Background(), f); err == nil {
			t.Fatalf("pull over a %d-byte local file must fail", len(local))
		}
		if got, _ := os.ReadFile(filepath.Join(dest, "a.bin")); !bytes.Equal(got, local) {
			t.Fatalf("local %d-byte file was overwritten", len(local))
		}
		if len(s.store.Ready()) != 1 {
			t.Fatal("relay deleted its copy although the pull was refused")
		}
	}

	// A file that appears at the final name during the download is kept too.
	dir := t.TempDir()
	part, final := filepath.Join(dir, ".x.part"), filepath.Join(dir, "x")
	os.WriteFile(part, []byte("new"), 0o644)
	os.WriteFile(final, []byte("old"), 0o644)
	if err := placeNoClobber(part, final); err == nil {
		t.Fatal("placeNoClobber replaced an existing file")
	}
	if got, _ := os.ReadFile(final); string(got) != "old" {
		t.Fatalf("final changed to %q", got)
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatal("partial file must be kept for a later attempt")
	}
}

func TestAuthQuotaAndNames(t *testing.T) {
	_, ts := newTestServer(t, 1<<20)
	if r, _ := http.Get(ts.URL + "/api/state"); r.StatusCode != 401 {
		t.Fatalf("state without login: %d", r.StatusCode)
	}
	if r, _ := http.Get(ts.URL + "/pull/list"); r.StatusCode != 401 {
		t.Fatalf("pull without token: %d", r.StatusCode)
	}
	c := login(t, ts)
	post := func(name string, size int) int {
		body, _ := json.Marshal(map[string]any{"Key": name, "Name": name, "Size": size})
		r, _ := c.Post(ts.URL+"/api/uploads", "application/json", bytes.NewReader(body))
		return r.StatusCode
	}
	for _, bad := range []string{"../etc/passwd", "/abs", ".hidden", "a/.b", ""} {
		name := bad
		if bad == "/abs" { // leading slash is stripped, so this one is legal
			continue
		}
		if code := post(name, 1); code != 400 {
			t.Errorf("name %q: want 400, got %d", bad, code)
		}
	}
	if code := post("big", 2<<20); code != http.StatusInsufficientStorage {
		t.Errorf("over quota: %d", code)
	}
	if code := post("ok", 100); code != 200 {
		t.Errorf("within quota: %d", code)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, ts := newTestServer(t, 0)
	var last int
	for i := 0; i < maxFailures+2; i++ {
		r, _ := http.Post(ts.URL+"/api/login", "application/json", bytes.NewReader([]byte(`{"Password":"nope"}`)))
		last = r.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("want 429 after repeated failures, got %d", last)
	}
}
