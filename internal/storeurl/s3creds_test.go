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

// The environment comes first, with its session token.
func TestAmbientFromTheEnvironment(t *testing.T) {
	c := &Chain{Getenv: env(map[string]string{"AWS_ACCESS_KEY_ID": "AK", "AWS_SECRET_ACCESS_KEY": "SK", "AWS_SESSION_TOKEN": "TOK"})}
	got, err := c.Retrieve(context.Background())
	if err != nil || got.AccessKeyID != "AK" || got.SessionToken != "TOK" || !got.Expires.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	c = &Chain{Getenv: env(map[string]string{"AWS_ACCESS_KEY_ID": "AK"})}
	if _, err := c.Retrieve(context.Background()); err == nil {
		t.Fatal("a key ID with no secret was accepted")
	}
}

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

func webIdentityChain(t *testing.T, f *fakeSTS) (*Chain, string) {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tokFile := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tokFile, []byte("jwt-1\n"), 0o600)
	return &Chain{
		Getenv:      env(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": tokFile, "AWS_ROLE_ARN": "arn:aws:iam::123456789012:role/c4"}),
		STSEndpoint: srv.URL,
		Now:         f.clk.Now,
	}, tokFile
}

// A pod's web identity: exchanged with STS, kept until close to expiry,
// renewed with the token file as it is then (the kubelet rotates it), and
// kept through a failed renewal while it still holds.
func TestAmbientWebIdentityRenews(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	f := &fakeSTS{clk: clk}
	c, tokFile := webIdentityChain(t, f)
	ctx := context.Background()

	got, err := c.Retrieve(ctx)
	if err != nil || got.AccessKeyID != "ASIA1" || got.SessionToken != "token1" {
		t.Fatalf("%+v %v", got, err)
	}
	clk.Add(50 * time.Minute) // 10 minutes left: still fresh
	if got, _ := c.Retrieve(ctx); got.AccessKeyID != "ASIA1" || f.calls.Load() != 1 {
		t.Fatalf("renewed early: %s, %d calls", got.AccessKeyID, f.calls.Load())
	}
	os.WriteFile(tokFile, []byte("jwt-2\n"), 0o600)
	clk.Add(6 * time.Minute) // 4 minutes left: renew
	if got, _ := c.Retrieve(ctx); got.AccessKeyID != "ASIA2" {
		t.Fatalf("not renewed: %s", got.AccessKeyID)
	}
	if f.tokens[1] != "jwt-2" {
		t.Fatalf("renewed with the old token file: %q", f.tokens)
	}

	// STS goes down near expiry: the keys that still hold are used.
	f.fail.Store(true)
	clk.Add(57 * time.Minute) // 3 minutes left
	if got, err := c.Retrieve(ctx); err != nil || got.AccessKeyID != "ASIA2" {
		t.Fatalf("lost keys that still hold: %+v %v", got, err)
	}
	clk.Add(4 * time.Minute) // expired, STS still down
	if _, err := c.Retrieve(ctx); err == nil || !strings.Contains(err.Error(), "ServiceUnavailable") {
		t.Fatalf("expired keys were used, or the error was lost: %v", err)
	}
	f.fail.Store(false)
	if got, err := c.Retrieve(ctx); err != nil || got.AccessKeyID != "ASIA3" {
		t.Fatalf("no recovery: %+v %v", got, err)
	}
}

// Many requests at once start one exchange: those holding no keys wait
// for it, and none start their own.
func TestAmbientRenewsOnceUnderLoad(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	f := &fakeSTS{clk: clk, delay: 50 * time.Millisecond}
	c, _ := webIdentityChain(t, f)
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := c.Retrieve(context.Background())
			if err == nil && got.AccessKeyID != "ASIA1" {
				err = fmt.Errorf("got %s", got.AccessKeyID)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d exchanges for 64 requests", n)
	}
}

// An EC2 instance's role, through IMDSv2: a session token first, then the
// role's keys, each asked with it.
func TestAmbientInstanceRole(t *testing.T) {
	exp := time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/latest/api/token" && r.Header.Get("X-Aws-Ec2-Metadata-Token-Ttl-Seconds") != "":
			io.WriteString(w, "imds-token")
		case r.Header.Get("X-Aws-Ec2-Metadata-Token") != "imds-token":
			w.WriteHeader(http.StatusUnauthorized)
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/":
			io.WriteString(w, "c4d-relay\n")
		case r.URL.Path == "/latest/meta-data/iam/security-credentials/c4d-relay":
			fmt.Fprintf(w, `{"Code":"Success","Type":"AWS-HMAC","AccessKeyId":"ASIAROLE","SecretAccessKey":"rs","Token":"rt","Expiration":%q}`, exp)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Chain{Getenv: env(nil), IMDSEndpoint: srv.URL}
	got, err := c.Retrieve(context.Background())
	if err != nil || got.AccessKeyID != "ASIAROLE" || got.SessionToken != "rt" || got.Expires.IsZero() {
		t.Fatalf("%+v %v", got, err)
	}
	c = &Chain{Getenv: env(map[string]string{"AWS_EC2_METADATA_DISABLED": "true"}), IMDSEndpoint: srv.URL}
	if _, err := c.Retrieve(context.Background()); err == nil {
		t.Fatal("used the metadata service it was told not to")
	}
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
	chain := &Chain{
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
