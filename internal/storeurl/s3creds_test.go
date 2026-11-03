package storeurl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"
)

// AWS's Signature Version 4 test suite (botocore's copy of aws4_testsuite,
// in testdata/aws4): the plain GET, and a POST signed with a session
// token, which is signed as X-Amz-Security-Token.
func TestSignV4MatchesAWSTestSuite(t *testing.T) {
	creds := Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"}
	when := time.Date(2015, 8, 30, 12, 36, 0, 0, time.UTC)
	empty := sha256.Sum256(nil)
	for _, name := range []string{"get-vanilla", "post-sts-header-before"} {
		raw, err := os.ReadFile(filepath.Join("testdata", "aws4", name+".req"))
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
		method, path, _ := strings.Cut(strings.Fields(lines[0])[0]+" "+strings.Fields(lines[0])[1], " ")
		req, _ := http.NewRequest(method, "https://example.amazonaws.com"+path, nil)
		c := creds
		for _, l := range lines[1:] {
			k, v, _ := strings.Cut(l, ":")
			if strings.EqualFold(k, "X-Amz-Security-Token") {
				c.SessionToken = v
			}
		}
		sign(req, c, "us-east-1", "service", when, hex.EncodeToString(empty[:]))
		want, _ := os.ReadFile(filepath.Join("testdata", "aws4", name+".authz"))
		if got := req.Header.Get("Authorization"); got != strings.TrimSpace(string(want)) {
			t.Errorf("%s:\n got %s\nwant %s", name, got, want)
		}
	}
}

// A request signed without a token carries none, even on a request
// that had one before (a retry re-signed after keys became static).
func TestNoTokenNoHeader(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://b.s3.us-east-1.amazonaws.com/k", nil)
	signV4(req, Credentials{AccessKeyID: "A", SecretAccessKey: "S", SessionToken: "T"}, "us-east-1", time.Now())
	if req.Header.Get("X-Amz-Security-Token") != "T" || !strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("token not signed: %v", req.Header)
	}
	signV4(req, Credentials{AccessKeyID: "A", SecretAccessKey: "S"}, "us-east-1", time.Now())
	if req.Header.Get("X-Amz-Security-Token") != "" || strings.Contains(req.Header.Get("Authorization"), "x-amz-security-token") {
		t.Fatalf("a stale token was sent: %v", req.Header)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// clock is a time the test moves.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) Add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

// fakeSTS answers AssumeRoleWithWebIdentity with keys numbered by call,
// valid for an hour of the test's clock.
type fakeSTS struct {
	clk    *clock
	calls  atomic.Int32
	tokens []string
	mu     sync.Mutex
	delay  time.Duration
	fail   atomic.Bool
}

func (f *fakeSTS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	if r.Method != http.MethodPost || r.Form.Get("Action") != "AssumeRoleWithWebIdentity" || r.Form.Get("RoleArn") != "arn:aws:iam::123456789012:role/c4" || r.Form.Get("RoleSessionName") == "" {
		http.Error(w, "<ErrorResponse><Error><Code>InvalidParameter</Code><Message>bad request</Message></Error></ErrorResponse>", 400)
		return
	}
	if f.fail.Load() {
		http.Error(w, "<ErrorResponse><Error><Code>ServiceUnavailable</Code><Message>down</Message></Error></ErrorResponse>", 503)
		return
	}
	time.Sleep(f.delay)
	f.mu.Lock()
	f.tokens = append(f.tokens, r.Form.Get("WebIdentityToken"))
	f.mu.Unlock()
	n := f.calls.Add(1)
	exp := f.clk.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	fmt.Fprintf(w, `<AssumeRoleWithWebIdentityResponse><AssumeRoleWithWebIdentityResult><Credentials>
<AccessKeyId>ASIA%d</AccessKeyId><SecretAccessKey>secret%d</SecretAccessKey><SessionToken>token%d</SessionToken><Expiration>%s</Expiration>
</Credentials></AssumeRoleWithWebIdentityResult></AssumeRoleWithWebIdentityResponse>`, n, n, n, exp)
}

// fakeS3 is a bucket that accepts only the keys STS gave most recently,
// with their token, and holds single and multipart uploads.
type fakeS3 struct {
	mu      sync.Mutex
	valid   map[string]string // access key -> token
	objects map[string][]byte
	uploads map[string]map[int][]byte
	aborted int
	fail503 int
	partErr int // fail this part number with 403
}

var credRE = regexp.MustCompile(`Credential=([^/]+)/`)

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m := credRE.FindStringSubmatch(r.Header.Get("Authorization"))
	if m == nil || f.valid[m[1]] == "" || r.Header.Get("X-Amz-Security-Token") != f.valid[m[1]] ||
		!strings.Contains(r.Header.Get("Authorization"), "x-amz-security-token") {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "<Error><Code>ExpiredToken</Code><Message>The provided token has expired.</Message></Error>")
		return
	}
	body, _ := io.ReadAll(r.Body)
	if r.Header.Get("X-Amz-Content-Sha256") != "UNSIGNED-PAYLOAD" {
		if sum := sha256.Sum256(body); r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "<Error><Code>XAmzContentSHA256Mismatch</Code><Message>hash</Message></Error>")
			return
		}
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket/")
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPut && f.fail503 > 0:
		f.fail503--
		w.WriteHeader(http.StatusServiceUnavailable)
	case r.Method == http.MethodHead:
		if b, ok := f.objects[key]; ok {
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	case r.Method == http.MethodPost && q.Has("uploads"):
		id := fmt.Sprintf("up%d", len(f.uploads)+1)
		f.uploads[id] = map[int][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		var n int
		fmt.Sscan(q.Get("partNumber"), &n)
		if n == f.partErr {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>no</Message></Error>")
			return
		}
		f.uploads[q.Get("uploadId")][n] = body
		w.Header().Set("ETag", fmt.Sprintf(`"etag-%d"`, n))
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		parts := f.uploads[q.Get("uploadId")]
		var all []byte
		for i := 1; i <= len(parts); i++ {
			if !bytes.Contains(body, []byte(fmt.Sprintf(`<PartNumber>%d</PartNumber><ETag>&#34;etag-%d&#34;</ETag>`, i, i))) {
				io.WriteString(w, "<Error><Code>InvalidPart</Code><Message>part list</Message></Error>")
				return
			}
			all = append(all, parts[i]...)
		}
		f.objects[key] = all
		io.WriteString(w, "<CompleteMultipartUploadResult></CompleteMultipartUploadResult>")
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		delete(f.uploads, q.Get("uploadId"))
		f.aborted++
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		f.objects[key] = body
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// A store on a pod's web identity keeps working as its keys expire mid-run:
// the bucket refuses the old keys once STS has issued new ones, and every
// request after the renewal is signed with the new keys and token.
func TestS3StoreSurvivesExpiryMidRun(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	bucket := &fakeS3{valid: map[string]string{}, objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	sts := &fakeSTS{clk: clk}
	stsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		sts.ServeHTTP(rec, r)
		// What STS issues, the bucket accepts; it forgets older keys.
		n := sts.calls.Load()
		bucket.mu.Lock()
		bucket.valid = map[string]string{fmt.Sprintf("ASIA%d", n): fmt.Sprintf("token%d", n)}
		bucket.mu.Unlock()
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		w.Write(rec.Body.Bytes())
	}))
	defer stsSrv.Close()
	s3Srv := httptest.NewServer(bucket)
	defer s3Srv.Close()
	tokFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokFile, []byte("jwt"), 0o600)
	chain := &store.Chain{
		Getenv:      env(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": tokFile, "AWS_ROLE_ARN": "arn:aws:iam::123456789012:role/c4"}),
		STSEndpoint: stsSrv.URL,
		Now:         clk.Now,
	}
	sh, err := OpenWith("s3://bucket/runs/?region=us-east-1&endpoint="+url.QueryEscape(s3Srv.URL), chain)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := range 4 {
		data := []byte(fmt.Sprintf("object %d", i))
		id := c4.Identify(bytes.NewReader(data))
		if err := sh.Put(ctx, id, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		if n, err := sh.Head(ctx, id); err != nil || n != int64(len(data)) {
			t.Fatalf("head %d: %d %v", i, n, err)
		}
		if _, ok := bucket.objects["runs/"+id.String()]; !ok {
			t.Fatalf("object %d not under the prefix", i)
		}
		clk.Add(40 * time.Minute) // across two renewals in four objects
	}
	if n := sts.calls.Load(); n < 2 {
		t.Fatalf("keys never renewed (%d exchanges)", n)
	}
}

func testBucket(t *testing.T) (*fakeS3, Shard) {
	bucket := &fakeS3{valid: map[string]string{"AK": "TOK"}, objects: map[string][]byte{}, uploads: map[string]map[int][]byte{}}
	srv := httptest.NewServer(bucket)
	t.Cleanup(srv.Close)
	sh, err := Open("s3://bucket/p/?endpoint="+url.QueryEscape(srv.URL), Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "TOK"})
	if err != nil {
		t.Fatal(err)
	}
	return bucket, sh
}

// Objects larger than a part go up in parts, which S3 joins in order; a
// part that fails aborts the upload, so no parts are left to bill.
func TestS3MultipartPut(t *testing.T) {
	defer func(p int64) { minPartSize = p }(minPartSize)
	minPartSize = 5
	bucket, sh := testBucket(t)
	ctx := context.Background()
	for _, n := range []int{0, 4, 5, 23, 25} {
		data := bytes.Repeat([]byte("abcdefg"), 4)[:n]
		id := c4.Identify(bytes.NewReader(data))
		for _, size := range []int64{int64(n), -1} {
			if err := sh.Put(ctx, id, bytes.NewReader(data), size); err != nil {
				t.Fatalf("%d bytes (size %d): %v", n, size, err)
			}
			if got := bucket.objects["p/"+id.String()]; !bytes.Equal(got, data) {
				t.Fatalf("%d bytes stored as %q", n, got)
			}
		}
	}
	data := []byte("0123456789abcdefghij")
	id := c4.Identify(bytes.NewReader(data))
	if err := sh.Put(ctx, id, bytes.NewReader(data), 21); err == nil {
		t.Fatal("a short read was stored")
	}
	bucket.partErr = 3
	if err := sh.Put(ctx, id, bytes.NewReader(data), 20); err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("failed part: %v", err)
	}
	if bucket.aborted != 2 {
		t.Fatalf("%d uploads aborted, want 2", bucket.aborted)
	}
}

// 503 SlowDown is tried again, re-signed, and succeeds.
func TestS3PutRetriesSlowDown(t *testing.T) {
	defer func(d time.Duration) { putBackoff = d }(putBackoff)
	putBackoff = time.Millisecond
	bucket, sh := testBucket(t)
	bucket.fail503 = 2
	data := []byte("slow")
	id := c4.Identify(bytes.NewReader(data))
	if err := sh.Put(context.Background(), id, bytes.NewReader(data), 4); err != nil {
		t.Fatal(err)
	}
	if bucket.fail503 != 0 || !bytes.Equal(bucket.objects["p/"+id.String()], data) {
		t.Fatal("not retried")
	}
}

// The Project CRD refuses the same URLs on apply (a CEL rule on the
// shard URL) that Open refuses: its regular expression, read from the API
// source, agrees with Open on every case.
func TestAdmissionAgreesWithOpen(t *testing.T) {
	src, err := os.ReadFile("../../api/v1alpha1/project_types.go")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`self\.matches\('([^']+)'\)`).FindSubmatch(src)
	if m == nil {
		t.Fatal("no shard URL rule in project_types.go")
	}
	rule := regexp.MustCompile(string(m[1]))
	var forbidden []string
	for _, f := range regexp.MustCompile(`!self\.contains\('([^']+)'\)`).FindAllSubmatch(src, -1) {
		forbidden = append(forbidden, string(f[1]))
	}
	if len(forbidden) == 0 {
		t.Fatal("no instance-role rule in project_types.go")
	}
	admit := func(raw string) bool {
		for _, f := range forbidden {
			if strings.Contains(raw, f) {
				return false
			}
		}
		return rule.MatchString(raw)
	}
	creds := Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}
	for _, raw := range []string{
		"s3://bucket", "s3://bucket/", "s3://bucket/runs/", "s3://bucket/a/b/",
		"s3://bucket/runs", "s3://bucket/c4-bench-x", "s3://bucket/a/b?region=us-west-2",
		"s3://bucket/a/b/?region=us-west-2&endpoint=http://minio:9000", "s3://bucket?region=us-west-2",
		"s3://bucket/runs/?instance-role=true", "s3://bucket/runs/?region=us-west-2&instance-role=false",
	} {
		_, err := Open(raw, creds)
		if admitted := admit(raw); admitted != (err == nil) {
			t.Errorf("%s: admission %v, open %v", raw, admitted, err)
		}
	}
}

// A prefix that doesn't end in "/" is refused: its objects would land
// beside the folder (runs<id>), where nothing that lists the folder finds
// them. The c4 CLI names the same keys for the same URL, so storeurl
// can't add the "/" itself.
func TestS3PrefixMustEndInSlash(t *testing.T) {
	creds := Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}
	for raw, ok := range map[string]bool{
		"s3://bucket":            true,
		"s3://bucket/":           true,
		"s3://bucket/runs/":      true,
		"s3://bucket/a/b/":       true,
		"s3://bucket/runs":       false,
		"s3://bucket/c4-bench-x": false,
	} {
		_, err := Open(raw, creds)
		if (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}
