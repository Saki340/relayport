package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	stallTimeout = 60 * time.Second
	maxAttempts  = 30
)

type puller struct {
	server  string
	token   string
	dest    string
	segSize int64
	slots   chan struct{} // bounds concurrent segment downloads across all files
	client  *http.Client

	mu     sync.Mutex
	active map[string]bool
}

// pullState is persisted next to the partial file so interrupted pulls resume.
type pullState struct {
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	SegSize int64  `json:"segSize"`
	Done    []bool `json:"done"`
}

func runPull(ctx context.Context) error {
	server := strings.TrimRight(os.Getenv("RELAYPORT_SERVER"), "/")
	if server == "" {
		return errors.New("RELAYPORT_SERVER must be set")
	}
	token, err := requireSecret("RELAYPORT_PULL_TOKEN")
	if err != nil {
		return err
	}
	seg, err := parseSize(envOr("RELAYPORT_SEGMENT", "16MiB"))
	if err != nil || seg <= 0 {
		return fmt.Errorf("RELAYPORT_SEGMENT: invalid")
	}
	par, err := strconv.Atoi(envOr("RELAYPORT_PARALLEL", "4"))
	if err != nil || par < 1 {
		return fmt.Errorf("RELAYPORT_PARALLEL: invalid")
	}
	files, err := strconv.Atoi(envOr("RELAYPORT_FILES", "2"))
	if err != nil || files < 1 {
		return fmt.Errorf("RELAYPORT_FILES: invalid")
	}
	poll, err := time.ParseDuration(envOr("RELAYPORT_POLL", "15s"))
	if err != nil {
		return fmt.Errorf("RELAYPORT_POLL: %w", err)
	}
	dest := envOr("RELAYPORT_DEST", "./inbox")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	p := &puller{
		server: server, token: token, dest: dest, segSize: seg,
		slots: make(chan struct{}, par),
		client: &http.Client{Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment, DisableCompression: true,
			MaxIdleConnsPerHost: par * 2, ResponseHeaderTimeout: stallTimeout,
		}},
		active: map[string]bool{},
	}
	log.Printf("pulling from %s into %s (%d segments of %s, %d files at a time)", server, dest, par, envOr("RELAYPORT_SEGMENT", "16MiB"), files)

	fileSlots := make(chan struct{}, files)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		list, err := p.list(ctx)
		if err != nil {
			log.Printf("list: %v", err)
		}
		for _, f := range list {
			if !p.claim(f.Name) {
				continue
			}
			select {
			case fileSlots <- struct{}{}:
			case <-ctx.Done():
				return nil
			}
			wg.Add(1)
			go func(f Staged) {
				defer wg.Done()
				defer func() { <-fileSlots; p.release(f.Name) }()
				if err := p.pullOne(ctx, f); err != nil && ctx.Err() == nil {
					log.Printf("%s: %v (will retry)", f.Name, err)
				}
			}(f)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(poll):
		}
	}
}

func (p *puller) claim(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.active[name] {
		return false
	}
	p.active[name] = true
	return true
}

func (p *puller) release(name string) {
	p.mu.Lock()
	delete(p.active, name)
	p.mu.Unlock()
}

func (p *puller) do(ctx context.Context, method, path string, body []byte, hdr map[string]string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, p.server+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return p.client.Do(req)
}

func (p *puller) list(ctx context.Context) ([]Staged, error) {
	resp, err := p.do(ctx, "GET", "/pull/list", nil, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("server said %s", resp.Status)
	}
	var out []Staged
	return out, json.NewDecoder(resp.Body).Decode(&out)
}

func (p *puller) post(ctx context.Context, path string, v any) error {
	b, _ := json.Marshal(v)
	resp, err := p.do(ctx, "POST", path, b, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (p *puller) pullOne(ctx context.Context, f Staged) error {
	final := filepath.Join(p.dest, filepath.FromSlash(f.Name))
	dir, base := filepath.Dir(final), filepath.Base(final)
	part, statePath := filepath.Join(dir, "."+base+".part"), filepath.Join(dir, "."+base+".state")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	// Either a finished file whose confirmation never reached the relay, or an
	// unrelated local file with the same name, which must never be overwritten.
	if info, err := os.Stat(final); err == nil {
		if info.Mode().IsRegular() && info.Size() == f.Size {
			if sum, err := hashFile(final); err == nil && sum == f.SHA256 {
				log.Printf("%s: already present, confirming", f.Name)
				return p.post(ctx, "/pull/done", map[string]string{"name": f.Name, "sha256": sum})
			}
		}
		return fmt.Errorf("%s exists locally with different content; move it away", final)
	}

	n := int((f.Size + p.segSize - 1) / p.segSize)
	st := pullState{Size: f.Size, SHA256: f.SHA256, SegSize: p.segSize, Done: make([]bool, n)}
	if b, err := os.ReadFile(statePath); err == nil {
		var old pullState
		if json.Unmarshal(b, &old) == nil && old.Size == st.Size && old.SHA256 == st.SHA256 &&
			old.SegSize == st.SegSize && len(old.Done) == n {
			st = old
		}
	}
	pf, err := os.OpenFile(part, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer pf.Close()
	if err := pf.Truncate(f.Size); err != nil {
		return err
	}

	var (
		mu       sync.Mutex
		received int64
	)
	for i, d := range st.Done {
		if d {
			received += segLen(f.Size, p.segSize, i)
		}
	}
	save := func() { // callers hold mu
		b, _ := json.Marshal(st)
		os.WriteFile(statePath, b, 0o644)
	}

	start := time.Now()
	log.Printf("%s: pulling %d bytes in %d segments (%d already done)", f.Name, f.Size, n, countTrue(st.Done))
	rctx, stopReport := context.WithCancel(ctx)
	reported := make(chan struct{})
	go func() {
		defer close(reported)
		for t := time.NewTicker(3 * time.Second); ; {
			select {
			case <-rctx.Done():
				return
			case <-t.C:
				mu.Lock()
				b := received
				mu.Unlock()
				p.post(rctx, "/pull/progress", map[string]any{"name": f.Name, "bytes": b})
			}
		}
	}()

	var wg sync.WaitGroup
	errc := make(chan error, 1)
	segCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := 0; i < n; i++ {
		if st.Done[i] {
			continue
		}
		select {
		case p.slots <- struct{}{}:
		case <-segCtx.Done():
		}
		if segCtx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			defer func() { <-p.slots }()
			if err := p.fetchSegment(segCtx, f, pf, i); err != nil {
				select {
				case errc <- fmt.Errorf("segment %d: %w", i, err):
				default:
				}
				cancel()
				return
			}
			mu.Lock()
			st.Done[i] = true
			received += segLen(f.Size, p.segSize, i)
			save()
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	stopReport()
	<-reported
	select {
	case err := <-errc:
		return err
	default:
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if err := pf.Sync(); err != nil {
		return err
	}
	sum, err := hashFile(part)
	if err != nil {
		return err
	}
	if sum != f.SHA256 {
		pf.Close()
		os.Remove(part)
		os.Remove(statePath)
		return fmt.Errorf("checksum mismatch after download, discarded and will start over")
	}
	pf.Close()
	if err := placeNoClobber(part, final); err != nil {
		return err // part and state are kept, so a later attempt only re-verifies
	}
	os.Remove(statePath)
	if err := p.post(ctx, "/pull/done", map[string]string{"name": f.Name, "sha256": sum}); err != nil {
		return fmt.Errorf("downloaded but confirmation failed: %w", err)
	}
	secs := time.Since(start).Seconds()
	log.Printf("%s: done in %.0fs (%.1f MiB/s)", f.Name, secs, float64(f.Size)/secs/(1<<20))
	return nil
}

// placeNoClobber moves part to final, failing if final already exists (another
// process may have created it while we were downloading). A hard link is atomic
// and refuses an existing target; filesystems without links fall back to rename.
func placeNoClobber(part, final string) error {
	err := os.Link(part, final)
	if err == nil {
		return os.Remove(part)
	}
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("%s appeared locally during the download; move it away", final)
	}
	if _, serr := os.Lstat(final); serr == nil {
		return fmt.Errorf("%s appeared locally during the download; move it away", final)
	}
	return os.Rename(part, final)
}

func countTrue(b []bool) (n int) {
	for _, v := range b {
		if v {
			n++
		}
	}
	return n
}

func segLen(size, seg int64, i int) int64 {
	if int64(i+1)*seg > size {
		return size - int64(i)*seg
	}
	return seg
}

// fetchSegment downloads one byte range into pf, retrying with backoff. A
// download that makes no progress for stallTimeout is abandoned and retried.
func (p *puller) fetchSegment(ctx context.Context, f Staged, pf *os.File, i int) error {
	off := int64(i) * p.segSize
	want := segLen(f.Size, p.segSize, i)
	var last error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if attempt > 0 {
			d := time.Duration(attempt) * 2 * time.Second
			if d > 30*time.Second {
				d = 30 * time.Second
			}
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if last = p.tryRange(ctx, f, pf, off, want); last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return last
}

func (p *puller) tryRange(ctx context.Context, f Staged, pf *os.File, off, want int64) error {
	if want == 0 {
		return nil
	}
	rctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()

	resp, err := p.do(rctx, "GET", "/pull/file?name="+url.QueryEscape(f.Name), nil,
		map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", off, off+want-1)})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("server said %s", resp.Status)
	}
	buf := make([]byte, 256<<10)
	var got int64
	for got < want {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			stall.Reset(stallTimeout)
			if int64(n) > want-got {
				n = int(want - got)
			}
			if _, werr := pf.WriteAt(buf[:n], off+got); werr != nil {
				return werr
			}
			got += int64(n)
		}
		if err != nil {
			if err == io.EOF && got == want {
				break
			}
			return fmt.Errorf("after %d of %d bytes: %w", got, want, err)
		}
	}
	return nil
}
