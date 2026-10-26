package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const defaultChunkSize = 8 << 20

var (
	errQuota    = errors.New("staging area is full")
	errExists   = errors.New("a file with this name is already staged")
	errBadPath  = errors.New("invalid file name")
	errNotFound = errors.New("not found")
)

// Upload is a resumable upload being assembled in the tmp area.
type Upload struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"` // relative, slash-separated
	Size      int64     `json:"size"`
	ChunkSize int64     `json:"chunkSize"`
	Done      []bool    `json:"done"`
	Updated   time.Time `json:"updated"`
	Hashing   bool      `json:"-"`
}

func (u *Upload) nChunks() int {
	return int((u.Size + u.ChunkSize - 1) / u.ChunkSize)
}

func (u *Upload) received() (n int64) {
	for i, ok := range u.Done {
		if ok {
			n += u.chunkLen(i)
		}
	}
	return n
}

func (u *Upload) chunkLen(i int) int64 {
	if int64(i+1)*u.ChunkSize > u.Size {
		return u.Size - int64(i)*u.ChunkSize
	}
	return u.ChunkSize
}

// Staged is a completed file in the inbox, waiting to be pulled.
type Staged struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size"`
	SHA256   string    `json:"sha256,omitempty"` // empty while hashing
	Pulled   int64     `json:"pulled"`           // bytes reported by the puller
	PullSeen time.Time `json:"-"`
}

// Store owns the staging area on disk.
type Store struct {
	root  string
	quota int64 // 0 = unlimited

	mu      sync.Mutex
	uploads map[string]*Upload
	staged  map[string]*Staged
}

func NewStore(root string, quota int64) (*Store, error) {
	s := &Store{root: root, quota: quota, uploads: map[string]*Upload{}, staged: map[string]*Staged{}}
	for _, d := range []string{s.inboxDir(), s.tmpDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, notWritable(root, err)
		}
		// Existing folders owned by someone else (e.g. root, from an older setup) would
		// otherwise only fail at the first upload.
		f, err := os.CreateTemp(d, ".probe-*")
		if err != nil {
			return nil, notWritable(root, err)
		}
		f.Close()
		os.Remove(f.Name())
	}
	s.loadUploads()
	s.scanInbox()
	return s, nil
}

func notWritable(root string, err error) error {
	if !errors.Is(err, fs.ErrPermission) {
		return err
	}
	return fmt.Errorf("%w: the data folder %s must be writable by uid %d gid %d; create it before the first start, "+
		"or give it to that user (sudo chown -R %d:%d <folder on the host>)",
		err, root, os.Getuid(), os.Getgid(), os.Getuid(), os.Getgid())
}

func (s *Store) inboxDir() string { return filepath.Join(s.root, "inbox") }
func (s *Store) tmpDir() string   { return filepath.Join(s.root, "tmp") }

func (s *Store) partPath(id string) string { return filepath.Join(s.tmpDir(), id+".part") }
func (s *Store) metaPath(id string) string { return filepath.Join(s.tmpDir(), id+".json") }

// CleanName validates a client-supplied relative name. Hidden path elements are
// refused so that clients can never collide with internal or partial files.
func CleanName(name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || len(name) > 1024 {
		return "", errBadPath
	}
	name = strings.TrimPrefix(name, "/")
	for _, el := range strings.Split(name, "/") {
		if el == "" || el == "." || el == ".." || strings.HasPrefix(el, ".") || strings.ContainsRune(el, 0) {
			return "", errBadPath
		}
	}
	return name, nil
}

func uploadID(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}

func (s *Store) loadUploads() {
	ents, _ := os.ReadDir(s.tmpDir())
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(s.tmpDir(), e.Name()))
		if err != nil {
			continue
		}
		var u Upload
		if json.Unmarshal(b, &u) != nil || len(u.Done) != u.nChunks() {
			continue
		}
		s.uploads[u.ID] = &u
	}
}

func (s *Store) scanInbox() {
	filepath.WalkDir(s.inboxDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(s.inboxDir(), p)
		rel = filepath.ToSlash(rel)
		st := &Staged{Name: rel, Size: info.Size()}
		s.staged[rel] = st
		go s.hashStaged(st) // not listed to pullers until hashed
		return nil
	})
}

// usedLocked is the space committed to staged files and reserved by uploads.
func (s *Store) usedLocked() (n int64) {
	for _, st := range s.staged {
		n += st.Size
	}
	for _, u := range s.uploads {
		n += u.Size
	}
	return n
}

// Create starts or resumes an upload identified by the client's key.
func (s *Store) Create(key, name string, size int64) (*Upload, error) {
	name, err := CleanName(name)
	if err != nil {
		return nil, err
	}
	if size < 0 {
		return nil, errBadPath
	}
	id := uploadID(key)
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.uploads[id]; ok && u.Name == name && u.Size == size {
		return u, nil
	}
	if _, ok := s.staged[name]; ok {
		return nil, errExists
	}
	for _, u := range s.uploads {
		if u.Name == name {
			return nil, errExists
		}
	}
	if s.quota > 0 && s.usedLocked()+size > s.quota {
		return nil, errQuota
	}
	u := &Upload{ID: id, Name: name, Size: size, ChunkSize: defaultChunkSize, Updated: time.Now()}
	u.Done = make([]bool, u.nChunks())
	f, err := os.OpenFile(s.partPath(id), os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, err
	}
	f.Close()
	s.uploads[id] = u
	return u, s.saveLocked(u)
}

func (s *Store) saveLocked(u *Upload) error {
	b, _ := json.Marshal(u)
	tmp := s.metaPath(u.ID) + ".new"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath(u.ID))
}

// WriteChunk stores chunk n of upload id, verifying wantSHA when non-empty.
func (s *Store) WriteChunk(id string, n int, r io.Reader, wantSHA string) error {
	s.mu.Lock()
	u, ok := s.uploads[id]
	if !ok || n < 0 || n >= len(u.Done) {
		s.mu.Unlock()
		return errNotFound
	}
	want := u.chunkLen(n)
	off := int64(n) * u.ChunkSize
	s.mu.Unlock()

	// Buffer the chunk so a dropped connection never leaves half a chunk marked done.
	buf := make([]byte, 0, want)
	b, err := io.ReadAll(io.LimitReader(r, want+1))
	if err != nil {
		return err
	}
	buf = append(buf, b...)
	if int64(len(buf)) != want {
		return fmt.Errorf("chunk %d: got %d bytes, want %d", n, len(buf), want)
	}
	if wantSHA != "" {
		sum := sha256.Sum256(buf)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), wantSHA) {
			return fmt.Errorf("chunk %d: checksum mismatch", n)
		}
	}
	f, err := os.OpenFile(s.partPath(id), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteAt(buf, off)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	u.Done[n] = true
	u.Updated = time.Now()
	return s.saveLocked(u)
}

// Complete moves a fully received upload into the inbox and starts hashing it.
func (s *Store) Complete(id string) (*Staged, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.uploads[id]
	if !ok {
		return nil, errNotFound
	}
	for i, d := range u.Done {
		if !d {
			return nil, fmt.Errorf("chunk %d missing", i)
		}
	}
	dst := filepath.Join(s.inboxDir(), filepath.FromSlash(u.Name))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(s.partPath(id), dst); err != nil {
		return nil, err
	}
	os.Remove(s.metaPath(id))
	delete(s.uploads, id)
	st := &Staged{Name: u.Name, Size: u.Size}
	s.staged[u.Name] = st
	go s.hashStaged(st)
	return st, nil
}

func (s *Store) hashStaged(st *Staged) {
	f, err := os.Open(filepath.Join(s.inboxDir(), filepath.FromSlash(st.Name)))
	if err != nil {
		return
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return
	}
	sum := hex.EncodeToString(h.Sum(nil))
	s.mu.Lock()
	st.SHA256 = sum
	s.mu.Unlock()
}

// Ready lists hashed files for pullers, smallest first.
func (s *Store) Ready() []Staged {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Staged
	for _, st := range s.staged {
		if st.SHA256 != "" {
			out = append(out, *st)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Size < out[j].Size })
	return out
}

// Snapshot is the state shown on the web page.
type Snapshot struct {
	Uploads []UploadView `json:"uploads"`
	Staged  []Staged     `json:"staged"`
	Used    int64        `json:"used"`
	Quota   int64        `json:"quota"`
}

type UploadView struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Received int64  `json:"received"`
}

func (s *Store) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn := Snapshot{Used: s.usedLocked(), Quota: s.quota, Uploads: []UploadView{}, Staged: []Staged{}}
	for _, u := range s.uploads {
		sn.Uploads = append(sn.Uploads, UploadView{u.ID, u.Name, u.Size, u.received()})
	}
	for _, st := range s.staged {
		sn.Staged = append(sn.Staged, *st)
	}
	sort.Slice(sn.Uploads, func(i, j int) bool { return sn.Uploads[i].Name < sn.Uploads[j].Name })
	sort.Slice(sn.Staged, func(i, j int) bool { return sn.Staged[i].Name < sn.Staged[j].Name })
	return sn
}

// Open returns a staged, hashed file for serving to a puller.
func (s *Store) Open(name string) (*os.File, *Staged, error) {
	name, err := CleanName(name)
	if err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	st, ok := s.staged[name]
	var cp Staged
	if ok {
		cp = *st
	}
	s.mu.Unlock()
	if !ok || cp.SHA256 == "" {
		return nil, nil, errNotFound
	}
	f, err := os.Open(filepath.Join(s.inboxDir(), filepath.FromSlash(name)))
	return f, &cp, err
}

// Progress records how many bytes the puller has fetched of a staged file.
func (s *Store) Progress(name string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.staged[name]; ok {
		st.Pulled, st.PullSeen = n, time.Now()
	}
}

// Finish deletes a staged file once the puller confirms its checksum.
func (s *Store) Finish(name, sum string) error {
	name, err := CleanName(name)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.staged[name]
	if !ok {
		return errNotFound
	}
	if st.SHA256 == "" || !strings.EqualFold(st.SHA256, sum) {
		return errors.New("checksum does not match the staged file")
	}
	p := filepath.Join(s.inboxDir(), filepath.FromSlash(name))
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	delete(s.staged, name)
	// Drop directories left empty so they are not recreated by pullers.
	for d := filepath.Dir(p); d != s.inboxDir() && strings.HasPrefix(d, s.inboxDir()); d = filepath.Dir(d) {
		if os.Remove(d) != nil {
			break
		}
	}
	return nil
}

// Sweep removes uploads that have not seen a chunk for longer than ttl.
func (s *Store) Sweep(ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, u := range s.uploads {
		if time.Since(u.Updated) > ttl {
			os.Remove(s.partPath(id))
			os.Remove(s.metaPath(id))
			delete(s.uploads, id)
		}
	}
}
