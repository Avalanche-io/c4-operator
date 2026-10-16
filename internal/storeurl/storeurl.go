// Package storeurl opens a content store from the same URL forms the c4
// CLI and the c4 container images accept in C4_STORE, so a Materialization
// can point at exactly the store a pipeline already writes to.
//
//	s3://bucket/prefix?region=us-west-2&endpoint=http://minio:9000
//	http://host:7433, https://host:7433, c4d://host:7433   (a c4d endpoint)
//	/path/to/store, file:///path/to/store                   (a c4 TreeStore)
package storeurl

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"
)

// Source is what the operator needs from a store: read by ID and ask
// whether an ID is present. Nothing here ever writes to the store.
type Source interface {
	Open(id c4.ID) (io.ReadCloser, error)
	Has(id c4.ID) bool
}

// Credentials carries S3 keys. Empty for stores that need none.
type Credentials struct {
	AccessKeyID     string
	SecretAccessKey string
	Endpoint        string // overrides the URL's endpoint query when set
}

// Open returns a read-only Source for raw.
func Open(raw string, creds Credentials) (Source, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "":
		return nil, fmt.Errorf("empty store URL")
	case strings.HasPrefix(raw, "s3://"):
		return openS3(raw, creds)
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
		return newC4D(raw), nil
	case strings.HasPrefix(raw, "c4d://"):
		return newC4D("http://" + strings.TrimPrefix(raw, "c4d://")), nil
	case strings.HasPrefix(raw, "file://"):
		return store.NewTreeStore(strings.TrimPrefix(raw, "file://"))
	case strings.HasPrefix(raw, "/"):
		return store.NewTreeStore(raw)
	}
	return nil, fmt.Errorf("unsupported store URL %q (want s3://, http(s)://, c4d://, or an absolute path)", raw)
}

// openS3 mirrors store.openS3Configured in the c4 module (unexported
// there) so object keys match what `c4 id -s` wrote: the key is the URL
// path with its leading slash trimmed, concatenated with the ID.
func openS3(raw string, creds Credentials) (Source, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("parse S3 URL: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("S3 URL missing bucket: %s", raw)
	}
	region := u.Query().Get("region")
	if region == "" {
		region = "us-east-1"
	}
	endpoint := u.Query().Get("endpoint")
	if creds.Endpoint != "" {
		endpoint = creds.Endpoint
	}
	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("S3 store %s needs credentials (storeSecretRef)", raw)
	}
	prefix := strings.TrimPrefix(u.Path, "/")
	return store.NewS3Store(u.Host, prefix, region, endpoint, creds.AccessKeyID, creds.SecretAccessKey), nil
}

// c4dSource reads from a c4d content endpoint: GET /<id>, HEAD /<id>.
type c4dSource struct {
	base   string
	client *http.Client
}

func newC4D(base string) *c4dSource {
	return &c4dSource{
		base:   strings.TrimSuffix(base, "/"),
		client: &http.Client{Timeout: 30 * time.Minute},
	}
}

func (s *c4dSource) Open(id c4.ID) (io.ReadCloser, error) {
	resp, err := s.client.Get(s.base + "/" + id.String())
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: status %d", id, resp.StatusCode)
	}
	return resp.Body, nil
}

func (s *c4dSource) Has(id c4.ID) bool {
	req, err := http.NewRequest(http.MethodHead, s.base+"/"+id.String(), nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
