package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const pushRetries = 8

// pusher uploads local files through the same chunk API as the web page, so an
// interrupted push resumes when it is run again (or when the file is dropped
// on the page, since the upload key is built the same way).
type pusher struct {
	server   string
	password string
	parallel int
	client   *http.Client
}

type pushItem struct {
	path, name string
	size       int64
	mod        time.Time
}

func newJar() *cookiejar.Jar {
	j, _ := cookiejar.New(nil)
	return j
}

func runPush(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("push", flag.ExitOnError)
	to := flags.String("to", "", "folder on the relay to put the files in")
	flags.Parse(args)
	if flags.NArg() == 0 {
		usage()
	}
	server, err := serverURL()
	if err != nil {
		return err
	}
	password, err := requireSecret("RELAYPORT_UPLOAD_PASSWORD")
	if err != nil {
		return err
	}
	par, err := strconv.Atoi(envOr("RELAYPORT_PARALLEL", "4"))
	if err != nil || par < 1 {
		return fmt.Errorf("RELAYPORT_PARALLEL: invalid")
	}
	items, err := collectPush(flags.Args(), *to)
	if err != nil {
		return err
	}
	p := newPusher(server, password, par)
	if err := p.login(ctx); err != nil {
		return err
	}
	failed := 0
	for _, it := range items {
		if err := p.pushOne(ctx, it); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			log.Printf("%s: %v", it.name, err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d files failed; run the same command again to resume", failed, len(items))
	}
	return nil
}

func newPusher(server, password string, parallel int) *pusher {
	return &pusher{server: server, password: password, parallel: parallel,
		client: &http.Client{Jar: newJar(), Timeout: 15 * time.Minute}}
}

// collectPush expands the arguments into files. A folder keeps its own name on
// the relay, like a folder dropped on the web page. Hidden files and folders
// are skipped, because the relay refuses hidden names.
func collectPush(args []string, to string) ([]pushItem, error) {
	var out []pushItem
	add := func(p, rel string, info fs.FileInfo) {
		name, err := CleanName(path.Join(to, filepath.ToSlash(rel)))
		if err != nil {
			log.Printf("skipping %s: not a valid relay name", p)
			return
		}
		out = append(out, pushItem{path: p, name: name, size: info.Size(), mod: info.ModTime()})
	}
	for _, arg := range args {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			add(abs, filepath.Base(abs), info)
			continue
		}
		parent := filepath.Dir(abs)
		err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p != abs && strings.HasPrefix(d.Name(), ".") {
				log.Printf("skipping hidden %s", p)
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(parent, p)
			add(p, rel, info)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// pushKey matches the key the web page uses, so either side can resume.
func pushKey(name string, size int64, mod time.Time) string {
	return fmt.Sprintf("%s|%d|%d", name, size, mod.UnixMilli())
}

func (p *pusher) request(ctx context.Context, method, path string, body []byte, hdr map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.server+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, b, err
}

func (p *pusher) login(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"Password": p.password})
	code, msg, err := p.request(ctx, "POST", "/api/login", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	switch code {
	case http.StatusNoContent:
		return nil
	case http.StatusUnauthorized:
		return errors.New("login: wrong RELAYPORT_UPLOAD_PASSWORD")
	case http.StatusTooManyRequests:
		return errors.New("login: the relay is refusing this address for 10 minutes after failed logins")
	}
	return fmt.Errorf("login: %d %s", code, strings.TrimSpace(string(msg)))
}

func (p *pusher) pushOne(ctx context.Context, it pushItem) error {
	body, _ := json.Marshal(map[string]any{"Key": pushKey(it.name, it.size, it.mod), "Name": it.name, "Size": it.size})
	code, msg, err := p.request(ctx, "POST", "/api/uploads", body, map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("%d %s", code, strings.TrimSpace(string(msg)))
	}
	var u struct {
		ID        string `json:"id"`
		ChunkSize int64  `json:"chunkSize"`
		Done      []bool `json:"done"`
	}
	if err := json.Unmarshal(msg, &u); err != nil || u.ChunkSize <= 0 {
		return fmt.Errorf("unexpected reply from relay: %s", msg)
	}
	f, err := os.Open(it.path)
	if err != nil {
		return err
	}
	defer f.Close()

	var sent atomic.Int64
	todo := make(chan int, len(u.Done))
	for i, d := range u.Done {
		if d {
			sent.Add(segLen(it.size, u.ChunkSize, i))
		} else {
			todo <- i
		}
	}
	close(todo)
	if len(todo) > 0 {
		log.Printf("%s: uploading %d bytes in %d chunks (%d already on the relay)", it.name, it.size, len(u.Done), len(u.Done)-len(todo))
	}

	start, base := time.Now(), sent.Load()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	for w := 0; w < p.parallel; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range todo {
				if cctx.Err() != nil {
					return
				}
				if err := p.putChunk(cctx, u.ID, f, i, u.ChunkSize, it.size); err != nil {
					errOnce.Do(func() { firstErr = fmt.Errorf("chunk %d: %w", i, err); cancel() })
					return
				}
				sent.Add(segLen(it.size, u.ChunkSize, i))
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				log.Printf("%s: %d / %d bytes", it.name, sent.Load(), it.size)
			}
		}
	}()
	wg.Wait()
	close(done)
	if firstErr != nil {
		return firstErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}

	code, msg, err = p.request(ctx, "POST", "/api/uploads/"+u.ID+"/complete", nil, nil)
	if err != nil {
		return err
	}
	if code != http.StatusOK {
		return fmt.Errorf("complete: %d %s", code, strings.TrimSpace(string(msg)))
	}
	if moved := sent.Load() - base; moved > 0 {
		log.Printf("%s: staged (%.1f MiB/s)", it.name, float64(moved)/time.Since(start).Seconds()/(1<<20))
	} else {
		log.Printf("%s: staged", it.name)
	}
	return nil
}

// putChunk uploads chunk i with its checksum, retrying with backoff.
func (p *pusher) putChunk(ctx context.Context, id string, f *os.File, i int, chunkSize, size int64) error {
	buf := make([]byte, segLen(size, chunkSize, i))
	if _, err := f.ReadAt(buf, int64(i)*chunkSize); err != nil {
		return err // the file shrank since it was listed
	}
	sum := sha256.Sum256(buf)
	hdr := map[string]string{"X-Chunk-Sha256": hex.EncodeToString(sum[:]), "Content-Type": "application/octet-stream"}
	var last error
	for attempt := 0; attempt <= pushRetries; attempt++ {
		if attempt > 0 {
			d := time.Second << (attempt - 1)
			if d > 20*time.Second {
				d = 20 * time.Second
			}
			select {
			case <-time.After(d):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		code, msg, err := p.request(ctx, "PUT", fmt.Sprintf("/api/uploads/%s/chunks/%d", id, i), buf, hdr)
		switch {
		case err != nil:
			last = err
		case code == http.StatusNoContent:
			return nil
		case code == http.StatusUnauthorized || code == http.StatusNotFound: // retrying cannot fix these
			return fmt.Errorf("%d %s", code, strings.TrimSpace(string(msg)))
		default:
			last = fmt.Errorf("%d %s", code, strings.TrimSpace(string(msg)))
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return last
}
