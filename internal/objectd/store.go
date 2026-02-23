package objectd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrForbidden = errors.New("forbidden")
)

type Store struct {
	mu       sync.RWMutex
	dataDir  string
	metaPath string
	state    metaState
}

type metaState struct {
	Buckets map[string]*bucketState `json:"buckets"`
}

type bucketState struct {
	CreatedAt string                  `json:"createdAt"`
	Objects   map[string]objectRecord `json:"objects"`
	Access    map[string]accessRecord `json:"access"`
}

type objectRecord struct {
	Size    int64  `json:"size"`
	ETag    string `json:"etag"`
	ModTime string `json:"modTime"`
	Path    string `json:"path"`
}

type accessRecord struct {
	SecretKey string `json:"secretKey"`
	ReadOnly  bool   `json:"readOnly"`
}

type Bucket struct {
	Name      string
	CreatedAt time.Time
}

type ObjectMeta struct {
	Bucket  string
	Key     string
	Size    int64
	ETag    string
	ModTime time.Time
	Path    string
}

type AccessKey struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
	Bucket    string `json:"bucket"`
	ReadOnly  bool   `json:"readOnly"`
}

func OpenStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dataDir, "objects"), 0o750); err != nil {
		return nil, err
	}
	s := &Store{
		dataDir:  dataDir,
		metaPath: filepath.Join(dataDir, "metadata.json"),
		state:    metaState{Buckets: map[string]*bucketState{}},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return nil }

func (s *Store) CreateBucket(_ context.Context, name string) error {
	if !validBucket(name) {
		return fmt.Errorf("invalid bucket name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Buckets[name]; ok {
		return nil
	}
	s.state.Buckets[name] = &bucketState{
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Objects:   map[string]objectRecord{},
		Access:    map[string]accessRecord{},
	}
	if err := os.MkdirAll(filepath.Join(s.dataDir, "objects", name), 0o750); err != nil {
		return err
	}
	return s.persistLocked()
}

func (s *Store) DeleteBucket(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.state.Buckets[name]
	if !ok {
		return ErrNotFound
	}
	if len(b.Objects) > 0 {
		return fmt.Errorf("bucket not empty")
	}
	delete(s.state.Buckets, name)
	if err := s.persistLocked(); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(s.dataDir, "objects", name))
}

func (s *Store) ListBuckets(_ context.Context) ([]Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Bucket, 0, len(s.state.Buckets))
	for name, b := range s.state.Buckets {
		t, _ := time.Parse(time.RFC3339Nano, b.CreatedAt)
		out = append(out, Bucket{Name: name, CreatedAt: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) PutObject(_ context.Context, bucket, key string, body io.Reader) (ObjectMeta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.state.Buckets[bucket]
	if !ok {
		return ObjectMeta{}, ErrNotFound
	}
	if key == "" {
		return ObjectMeta{}, fmt.Errorf("empty key")
	}
	if err := os.MkdirAll(filepath.Join(s.dataDir, "objects", bucket), 0o750); err != nil {
		return ObjectMeta{}, err
	}
	id, err := randomHex(24)
	if err != nil {
		return ObjectMeta{}, err
	}
	path := filepath.Join(s.dataDir, "objects", bucket, id)
	f, err := os.Create(path)
	if err != nil {
		return ObjectMeta{}, err
	}
	h := sha256.New()
	n, cpErr := io.Copy(io.MultiWriter(f, h), body)
	closeErr := f.Close()
	if cpErr != nil {
		_ = os.Remove(path)
		return ObjectMeta{}, cpErr
	}
	if closeErr != nil {
		_ = os.Remove(path)
		return ObjectMeta{}, closeErr
	}
	etag := hex.EncodeToString(h.Sum(nil))
	now := time.Now().UTC()

	if prev, ok := b.Objects[key]; ok && prev.Path != path {
		_ = os.Remove(prev.Path)
	}
	b.Objects[key] = objectRecord{Size: n, ETag: etag, ModTime: now.Format(time.RFC3339Nano), Path: path}
	if err := s.persistLocked(); err != nil {
		return ObjectMeta{}, err
	}
	return ObjectMeta{Bucket: bucket, Key: key, Size: n, ETag: etag, ModTime: now, Path: path}, nil
}

func (s *Store) GetObjectMeta(_ context.Context, bucket, key string) (ObjectMeta, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.state.Buckets[bucket]
	if !ok {
		return ObjectMeta{}, ErrNotFound
	}
	rec, ok := b.Objects[key]
	if !ok {
		return ObjectMeta{}, ErrNotFound
	}
	t, _ := time.Parse(time.RFC3339Nano, rec.ModTime)
	return ObjectMeta{Bucket: bucket, Key: key, Size: rec.Size, ETag: rec.ETag, ModTime: t, Path: rec.Path}, nil
}

func (s *Store) OpenObject(ctx context.Context, bucket, key string) (ObjectMeta, *os.File, error) {
	m, err := s.GetObjectMeta(ctx, bucket, key)
	if err != nil {
		return ObjectMeta{}, nil, err
	}
	f, err := os.Open(m.Path)
	if errors.Is(err, os.ErrNotExist) {
		return ObjectMeta{}, nil, ErrNotFound
	}
	return m, f, err
}

func (s *Store) DeleteObject(_ context.Context, bucket, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.state.Buckets[bucket]
	if !ok {
		return ErrNotFound
	}
	rec, ok := b.Objects[key]
	if !ok {
		return nil
	}
	delete(b.Objects, key)
	if err := s.persistLocked(); err != nil {
		return err
	}
	_ = os.Remove(rec.Path)
	return nil
}

func (s *Store) ListObjectsV2(_ context.Context, bucket, prefix, token string, maxKeys int) ([]ObjectMeta, string, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.state.Buckets[bucket]
	if !ok {
		return nil, "", false, ErrNotFound
	}
	if maxKeys <= 0 || maxKeys > 1000 {
		maxKeys = 1000
	}
	keys := make([]string, 0, len(b.Objects))
	for k := range b.Objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	start := 0
	if token != "" {
		for i, k := range keys {
			if k <= token {
				start = i + 1
			}
		}
	}
	keys = keys[start:]
	truncated := false
	next := ""
	if len(keys) > maxKeys {
		truncated = true
		next = keys[maxKeys-1]
		keys = keys[:maxKeys]
	}
	out := make([]ObjectMeta, 0, len(keys))
	for _, k := range keys {
		rec := b.Objects[k]
		t, _ := time.Parse(time.RFC3339Nano, rec.ModTime)
		out = append(out, ObjectMeta{Bucket: bucket, Key: k, Size: rec.Size, ETag: rec.ETag, ModTime: t, Path: rec.Path})
	}
	return out, next, truncated, nil
}

func (s *Store) CreateAccess(_ context.Context, bucket string, readOnly bool) (AccessKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Buckets[bucket]; !ok {
		return AccessKey{}, ErrNotFound
	}
	akRaw, err := randomHex(10)
	if err != nil {
		return AccessKey{}, err
	}
	sk, err := randomHex(32)
	if err != nil {
		return AccessKey{}, err
	}
	ak := "PX" + strings.ToUpper(akRaw)
	a := AccessKey{AccessKey: ak, SecretKey: sk, Bucket: bucket, ReadOnly: readOnly}
	if err := s.putAccessLocked(a); err != nil {
		return AccessKey{}, err
	}
	return a, nil
}

func (s *Store) PutAccess(_ context.Context, a AccessKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.putAccessLocked(a)
}

func (s *Store) putAccessLocked(a AccessKey) error {
	b, ok := s.state.Buckets[a.Bucket]
	if !ok {
		return ErrNotFound
	}
	b.Access[a.AccessKey] = accessRecord{SecretKey: a.SecretKey, ReadOnly: a.ReadOnly}
	return s.persistLocked()
}

func (s *Store) DeleteAccess(_ context.Context, accessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.state.Buckets {
		if _, ok := b.Access[accessKey]; ok {
			delete(b.Access, accessKey)
			return s.persistLocked()
		}
	}
	return nil
}

func (s *Store) LookupAccessKey(_ context.Context, accessKey string) (AccessKey, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for bucket, b := range s.state.Buckets {
		if rec, ok := b.Access[accessKey]; ok {
			return AccessKey{AccessKey: accessKey, SecretKey: rec.SecretKey, Bucket: bucket, ReadOnly: rec.ReadOnly}, nil
		}
	}
	return AccessKey{}, ErrNotFound
}

func (s *Store) load() error {
	b, err := os.ReadFile(s.metaPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, &s.state)
}

func (s *Store) persistLocked() error {
	tmp := s.metaPath + ".tmp"
	b, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.metaPath)
}

func validBucket(name string) bool {
	if len(name) < 3 || len(name) > 63 {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return false
	}
	for _, ch := range name {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '.' {
			continue
		}
		return false
	}
	return true
}

func randomHex(bytesN int) (string, error) {
	b := make([]byte, bytesN)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
