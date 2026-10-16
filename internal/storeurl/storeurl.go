// Package storeurl opens authoritative content stores from the URL forms
// the c4 CLI and the c4 container images accept in C4_STORE, and composes
// several of them into one sharded store.
//
//	s3://bucket/prefix?region=us-west-2&endpoint=http://minio:9000
//	c4d://host:7433, http://host:7433, https://host:7433   (c4d content API)
//	/path/to/store, file:///path/to/store                   (a c4 TreeStore)
//
// Reads honor offsets where the backend can (S3 and TreeStore), so an
// interrupted transfer resumes instead of restarting. Existence checks
// distinguish "absent" from "unreachable", which c4's S3Store.Has does
// not.
package storeurl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"

	"github.com/Avalanche-io/c4-operator/internal/rendezvous"
)

// ErrNotFound means the store answered that it does not hold the object.
var ErrNotFound = errors.New("not in store")

// Shard is one authoritative store.
type Shard interface {
	// Name identifies the shard (its URL without credentials).
	Name() string
	// Get reads id from offset. total is the object's size. start is where
	// the body begins: offset, or 0 when the backend cannot seek.
	Get(ctx context.Context, id c4.ID, offset int64) (body io.ReadCloser, total, start int64, err error)
	// Head returns the object's size, or ErrNotFound.
	Head(ctx context.Context, id c4.ID) (int64, error)
	// Put writes size bytes from r under id. The caller computed id; only
	// trusted ingest points call Put.
	Put(ctx context.Context, id c4.ID, r io.Reader, size int64) error
}

// Credentials carries S3 keys. Empty for stores that need none.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// Open returns the shard for one URL.
func Open(raw string, creds Credentials) (Shard, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return nil, errors.New("empty store URL")
	case strings.HasPrefix(raw, "s3://"):
		return openS3(raw, creds)
	case strings.HasPrefix(raw, "c4d://"):
		return newC4d("http://" + strings.TrimPrefix(raw, "c4d://")), nil
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
		return newC4d(raw), nil
	case strings.HasPrefix(raw, "file://"):
		return openTree(strings.TrimPrefix(raw, "file://"))
	case strings.HasPrefix(raw, "/"):
		return openTree(raw)
	}
	return nil, fmt.Errorf("unsupported store URL %q (want s3://, c4d://, http(s)://, or an absolute path)", raw)
}

// Sharded spreads objects over several shards by rendezvous hashing on the
// ID. Adding a shard moves about 1/(n+1) of the objects.
type Sharded struct {
	names  []string
	byName map[string]Shard
}

// OpenSharded opens every shard.
func OpenSharded(urls []string, creds Credentials) (*Sharded, error) {
	if len(urls) == 0 {
		return nil, errors.New("no store shards")
	}
	s := &Sharded{byName: map[string]Shard{}}
	for _, u := range urls {
		sh, err := Open(u, creds)
		if err != nil {
			return nil, fmt.Errorf("shard %s: %w", Redact(u), err)
		}
		if _, dup := s.byName[sh.Name()]; dup {
			return nil, fmt.Errorf("shard %s is listed twice", sh.Name())
		}
		s.names = append(s.names, sh.Name())
		s.byName[sh.Name()] = sh
	}
	return s, nil
}

// For returns the shard that holds id.
func (s *Sharded) For(id c4.ID) Shard {
	return s.byName[rendezvous.Rank(s.names, id[:])[0]]
}

// Shards lists the shards in configuration order.
func (s *Sharded) Shards() []Shard {
	out := make([]Shard, len(s.names))
	for i, n := range s.names {
		out[i] = s.byName[n]
	}
	return out
}

// Open reads a whole object (view.Store). It does not rehash.
func (s *Sharded) Open(id c4.ID) (io.ReadCloser, error) {
	body, _, _, err := s.For(id).Get(context.Background(), id, 0)
	return body, err
}

// Head asks id's shard.
func (s *Sharded) Head(ctx context.Context, id c4.ID) (int64, error) {
	return s.For(id).Head(ctx, id)
}

// Put writes to id's shard.
func (s *Sharded) Put(ctx context.Context, id c4.ID, r io.Reader, size int64) error {
	return s.For(id).Put(ctx, id, r, size)
}

// PutBytes stores small content (records, changesets) under its computed
// ID, unless the shard already has it.
func (s *Sharded) PutBytes(ctx context.Context, data []byte) (c4.ID, error) {
	id := c4.Identify(bytes.NewReader(data))
	if _, err := s.Head(ctx, id); err == nil {
		return id, nil
	} else if !errors.Is(err, ErrNotFound) {
		return id, err
	}
	return id, s.Put(ctx, id, bytes.NewReader(data), int64(len(data)))
}

// Redact removes credentials and query secrets from a URL for logs.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable URL>"
	}
	u.User = nil
	return u.String()
}

// ---- S3 ----

type s3Shard struct {
	name     string
	bucket   string
	prefix   string
	region   string
	endpoint string
	creds    Credentials
	client   *http.Client
	writer   *store.S3Store // multipart uploads
}

// openS3 mirrors store.openS3Configured in the c4 module so object keys
// match what `c4 id -s` wrote: the key is the URL path with its leading
// slash trimmed, concatenated with the ID.
func openS3(raw string, creds Credentials) (Shard, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse S3 URL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("S3 URL %q has no bucket", raw)
	}
	region := u.Query().Get("region")
	if region == "" {
		region = "us-east-1"
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("S3 store %s needs credentials", Redact(raw))
	}
	endpoint := u.Query().Get("endpoint")
	prefix := strings.TrimPrefix(u.Path, "/")
	return &s3Shard{
		name:     Redact(raw),
		bucket:   u.Host,
		prefix:   prefix,
		region:   region,
		endpoint: endpoint,
		creds:    creds,
		client: &http.Client{Transport: &http.Transport{
			MaxIdleConnsPerHost:   64,
			ResponseHeaderTimeout: 60 * time.Second,
			IdleConnTimeout:       90 * time.Second,
		}},
		writer: store.NewS3Store(u.Host, prefix, region, endpoint, creds.AccessKeyID, creds.SecretAccessKey),
	}, nil
}

func (s *s3Shard) Name() string { return s.name }

func (s *s3Shard) objectURL(id c4.ID) string {
	key := s.prefix + id.String()
	if s.endpoint != "" {
		scheme, ep := "https", s.endpoint
		if strings.HasPrefix(ep, "http://") {
			scheme, ep = "http", strings.TrimPrefix(ep, "http://")
		} else {
			ep = strings.TrimPrefix(ep, "https://")
		}
		return scheme + "://" + strings.TrimSuffix(ep, "/") + "/" + s.bucket + "/" + uriEncodePath(key)
	}
	return "https://" + s.bucket + ".s3." + s.region + ".amazonaws.com/" + uriEncodePath(key)
}

func (s *s3Shard) do(ctx context.Context, method string, id c4.ID, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.objectURL(id), nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	signV4(req, s.creds, s.region, time.Now().UTC())
	return s.client.Do(req)
}

func (s *s3Shard) Get(ctx context.Context, id c4.ID, offset int64) (io.ReadCloser, int64, int64, error) {
	hdr := http.Header{}
	if offset > 0 {
		hdr.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := s.do(ctx, http.MethodGet, id, hdr)
	if err != nil {
		return nil, 0, 0, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, resp.ContentLength, 0, nil
	case http.StatusPartialContent:
		start, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			resp.Body.Close()
			return nil, 0, 0, err
		}
		return resp.Body, total, start, nil
	case http.StatusRequestedRangeNotSatisfiable:
		resp.Body.Close()
		return nil, 0, 0, fmt.Errorf("s3 get %s: range %d not satisfiable", id, offset)
	}
	return nil, 0, 0, s3Error(resp, "get", id)
}

func (s *s3Shard) Head(ctx context.Context, id c4.ID) (int64, error) {
	resp, err := s.do(ctx, http.MethodHead, id, nil)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return resp.ContentLength, nil
	}
	return 0, s3Error(resp, "head", id)
}

func (s *s3Shard) Put(ctx context.Context, id c4.ID, r io.Reader, size int64) error {
	// c4's S3Store does multipart uploads for large objects.
	w, err := s.writer.Create(id)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}

func s3Error(resp *http.Response, op string, id c4.ID) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("s3 %s %s: %w", op, id, ErrNotFound)
	}
	msg := strings.TrimSpace(string(body))
	if i := strings.Index(msg, "<Message>"); i >= 0 {
		if j := strings.Index(msg[i:], "</Message>"); j > 0 {
			msg = msg[i+9 : i+j]
		}
	}
	return fmt.Errorf("s3 %s %s: status %d: %s", op, id, resp.StatusCode, msg)
}

func parseContentRange(v string) (start, total int64, err error) {
	// bytes START-END/TOTAL
	var end int64
	if _, err := fmt.Sscanf(v, "bytes %d-%d/%d", &start, &end, &total); err != nil {
		return 0, 0, fmt.Errorf("bad Content-Range %q", v)
	}
	return start, total, nil
}

// ---- c4d ----

type c4dShard struct {
	base   string
	client *http.Client
}

func newC4d(base string) *c4dShard {
	return &c4dShard{base: strings.TrimSuffix(base, "/"), client: &http.Client{Transport: &http.Transport{
		MaxIdleConnsPerHost:   32,
		ResponseHeaderTimeout: 60 * time.Second,
	}}}
}

func (s *c4dShard) Name() string { return s.base }

// Get reads from c4d's content plane. c4d does not honor Range, so a
// resumed read starts over at zero and says so.
func (s *c4dShard) Get(ctx context.Context, id c4.ID, offset int64) (io.ReadCloser, int64, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.base+"/"+id.String(), nil)
	if err != nil {
		return nil, 0, 0, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.Body, resp.ContentLength, 0, nil
	case http.StatusPartialContent:
		start, total, err := parseContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			resp.Body.Close()
			return nil, 0, 0, err
		}
		return resp.Body, total, start, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, 0, 0, fmt.Errorf("c4d %s: %w", id, ErrNotFound)
	}
	resp.Body.Close()
	return nil, 0, 0, fmt.Errorf("c4d get %s: status %d", id, resp.StatusCode)
}

func (s *c4dShard) Head(ctx context.Context, id c4.ID) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, s.base+"/"+id.String(), nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return resp.ContentLength, nil
	case http.StatusNotFound:
		return 0, fmt.Errorf("c4d %s: %w", id, ErrNotFound)
	}
	return 0, fmt.Errorf("c4d head %s: status %d", id, resp.StatusCode)
}

// Put sends the content to c4d, which computes the ID itself; the reply
// must match the ID the caller computed.
func (s *c4dShard) Put(ctx context.Context, id c4.ID, r io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, s.base+"/", r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("c4d put %s: status %d: %s", id, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if got := strings.TrimSpace(string(body)); got != id.String() {
		return fmt.Errorf("c4d stored %s as %s", id, got)
	}
	return nil
}

// ---- TreeStore ----

type treeShard struct {
	root string
	ts   *store.TreeStore
}

func openTree(root string) (Shard, error) {
	ts, err := store.NewTreeStore(root)
	if err != nil {
		return nil, err
	}
	return &treeShard{root: root, ts: ts}, nil
}

func (s *treeShard) Name() string { return "file://" + s.root }

func (s *treeShard) Get(_ context.Context, id c4.ID, offset int64) (io.ReadCloser, int64, int64, error) {
	p, ok := s.ts.ContentPath(id)
	if !ok {
		return nil, 0, 0, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, 0, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, 0, err
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		f.Close()
		return nil, 0, 0, err
	}
	return f, st.Size(), offset, nil
}

func (s *treeShard) Head(_ context.Context, id c4.ID) (int64, error) {
	p, ok := s.ts.ContentPath(id)
	if !ok {
		return 0, fmt.Errorf("%s: %w", id, ErrNotFound)
	}
	st, err := os.Stat(p)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

func (s *treeShard) Put(_ context.Context, id c4.ID, r io.Reader, _ int64) error {
	if s.ts.Has(id) {
		_, err := io.Copy(io.Discard, r)
		return err
	}
	w, err := s.ts.Create(id)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, r); err != nil {
		w.Close()
		return err
	}
	return w.Close()
}
