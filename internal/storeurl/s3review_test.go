package storeurl

// The findings of the security review of temporary S3 credentials
// (2026-11-03), each as a test that fails if the finding returns. They
// began as the reviewer's probes.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
)

const reviewToken = "FwoGZXIvYXdzEXAMPLESESSIONTOKEN-probe-secret-value"

// An S3 error body can quote the signed request, session token included,
// and is cut off when long. No error carries any of it: S3's Code and
// Message only.
func TestS3ErrorsNeverCarryTheBody(t *testing.T) {
	body := "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<Error><Code>SignatureDoesNotMatch</Code>" +
		"<Message>The request signature we calculated does not match the signature you provided.</Message>" +
		"<AWSAccessKeyId>ASIAPROBEKEY</AWSAccessKeyId><StringToSign>AWS4-HMAC-SHA256...</StringToSign>" +
		"<SignatureProvided>abc</SignatureProvided><StringToSignBytes>" + strings.Repeat("41 57 53 34 ", 120) + "</StringToSignBytes>" +
		"<CanonicalRequest>PUT\n/bucket/p/x\n\nhost:h\nx-amz-security-token:" + reviewToken +
		"\n\nhost;x-amz-security-token\n..</CanonicalRequest>" +
		"<CanonicalRequestBytes>" + strings.Repeat("50 55 54 0a ", 600) + "</CanonicalRequestBytes><RequestId>R</RequestId></Error>"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, body)
	}))
	defer srv.Close()
	sh, err := Open("s3://bucket/p/?endpoint="+url.QueryEscape(srv.URL), Credentials{AccessKeyID: "ASIAPROBEKEY", SecretAccessKey: "SK", SessionToken: reviewToken})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("x")
	id := c4.Identify(bytes.NewReader(data))
	_, herr := sh.Head(context.Background(), id)
	for op, err := range map[string]error{"put": sh.Put(context.Background(), id, bytes.NewReader(data), 1), "head": herr} {
		if err == nil {
			t.Fatalf("%s: no error", op)
		}
		for _, secret := range []string{reviewToken, "ASIAPROBEKEY", "CanonicalRequest"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s error carries %q: %v", op, secret, err)
			}
		}
	}
	// Even cut off, the code and message are still found, alone.
	if got := s3Message([]byte(body[:300])); got != ": SignatureDoesNotMatch: The request signature we calculated does not match the signature you provided." {
		t.Errorf("message from a cut-off body: %q", got)
	}
}

// No client follows a redirect: not S3 (the token and the body would go
// to the Location's host, and a 302 would turn a PUT into a body-less
// GET that "succeeds"), not STS (the web identity token would be re-posted,
// even to plain http), not the metadata service (its token would go too).
func TestNoRedirectIsFollowed(t *testing.T) {
	var saw atomic.Value
	saw.Store("")
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		saw.Store(r.Method + " " + r.Header.Get("X-Amz-Security-Token") + r.Form.Get("WebIdentityToken") + r.Header.Get("X-Aws-Ec2-Metadata-Token"))
		io.ReadAll(r.Body)
		io.WriteString(w, "ok")
	}))
	defer evil.Close()
	elsewhere := strings.Replace(evil.URL, "127.0.0.1", "localhost", 1)

	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusFound} {
		s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, elsewhere+r.URL.Path, code)
		}))
		sh, _ := Open("s3://bucket/p/?endpoint="+url.QueryEscape(s3.URL), Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: reviewToken})
		data := []byte("important bytes")
		err := sh.Put(context.Background(), c4.Identify(bytes.NewReader(data)), bytes.NewReader(data), int64(len(data)))
		s3.Close()
		if err == nil {
			t.Errorf("S3 %d: a redirected PUT counted as stored", code)
		}
		if saw.Load() != "" {
			t.Errorf("S3 %d: followed to %q", code, saw.Load())
		}
	}

	sts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, evil.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer sts.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("eyJ-probe-jwt"), 0o600)
	c := &Chain{Getenv: env(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": tok, "AWS_ROLE_ARN": "arn:aws:iam::1:role/r"}), STSEndpoint: sts.URL, Client: sts.Client()}
	if _, err := c.Retrieve(context.Background()); err == nil {
		t.Error("STS: a redirect gave credentials")
	}
	if saw.Load() != "" {
		t.Errorf("STS: followed to %q", saw.Load())
	}

	imds := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			io.WriteString(w, "imds-session")
			return
		}
		http.Redirect(w, r, elsewhere+r.URL.Path, http.StatusFound)
	}))
	defer imds.Close()
	c = &Chain{InstanceRole: true, Getenv: env(nil), IMDSEndpoint: imds.URL}
	if _, err := c.Retrieve(context.Background()); err == nil {
		t.Error("IMDS: a redirect gave credentials")
	}
	if saw.Load() != "" {
		t.Errorf("IMDS: followed to %q", saw.Load())
	}
}

// Keys that arrive already expired (a skewed clock, a bad endpoint) are an
// error, not keys; and the callers that meet the failure don't each start
// their own exchange.
func TestExpiredOnArrivalIsAnError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		exp := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `<R><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>ASIA%d</AccessKeyId><SecretAccessKey>s</SecretAccessKey><SessionToken>t</SessionToken><Expiration>%s</Expiration></Credentials></AssumeRoleWithWebIdentityResult></R>`, n, exp)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("jwt"), 0o600)
	c := &Chain{Getenv: env(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": tok, "AWS_ROLE_ARN": "arn:aws:iam::1:role/r"}), STSEndpoint: srv.URL}
	if got, err := c.Retrieve(context.Background()); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired keys: %v, %v", got, err)
	}
	calls.Store(0)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Retrieve(context.Background()); err == nil {
				t.Error("expired keys handed out")
			}
		}()
	}
	wg.Wait()
	if n := calls.Load(); n > 1 {
		t.Fatalf("%d exchanges for 16 callers after a failure", n)
	}
}

// A caller whose keys still hold never waits for the renewal it starts,
// and a caller with none waits only as long as its context.
func TestRenewalNeverHoldsACaller(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)}
	f := &fakeSTS{clk: clk}
	c, _ := webIdentityChain(t, f)
	if _, err := c.Retrieve(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.delay = 1500 * time.Millisecond
	clk.Add(56 * time.Minute) // 4 minutes left: due for renewal
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	got, err := c.Retrieve(ctx)
	if el := time.Since(start); el > 50*time.Millisecond || err != nil || got.AccessKeyID != "ASIA1" {
		t.Fatalf("with keys that hold: %v, %s, %v", el, got.AccessKeyID, err)
	}
	clk.Add(5 * time.Minute) // expired, renewal still running
	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	start = time.Now()
	if _, err := c.Retrieve(ctx2); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("with none: %v after %v", err, time.Since(start))
	}
}

// A small object of known size doesn't allocate a whole part.
func TestSmallPutAllocatesLittle(t *testing.T) {
	_, sh := testBucket(t)
	data := []byte("hello, small object")
	id := c4.Identify(bytes.NewReader(data))
	sh.Put(context.Background(), id, bytes.NewReader(data), int64(len(data)))
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	for range 10 {
		if err := sh.Put(context.Background(), id, bytes.NewReader(data), int64(len(data))); err != nil {
			t.Fatal(err)
		}
	}
	runtime.ReadMemStats(&b)
	if per := (b.TotalAlloc - a.TotalAlloc) / 10; per > 1<<20 {
		t.Fatalf("a %d-byte Put allocates %.1f MiB", len(data), float64(per)/(1<<20))
	}
}

// An upload that can't be finished is aborted on every path out: an
// error, a cancelled context, a panic in the reader. An abort that fails
// says which upload is left. A lost reply to starting an upload isn't
// retried, which would leave a second one open.
func TestUploadsAreNeverLeftSilently(t *testing.T) {
	defer func(p int64) { minPartSize = p }(minPartSize)
	defer func(d time.Duration) { putBackoff = d }(putBackoff)
	minPartSize, putBackoff = 5, time.Millisecond
	data := []byte("0123456789abcdefghij")
	id := c4.Identify(bytes.NewReader(data))

	t.Run("failed abort", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			switch {
			case r.Method == http.MethodPost && q.Has("uploads"):
				io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>U1</UploadId></InitiateMultipartUploadResult>")
			case r.Method == http.MethodPut && q.Get("partNumber") == "2":
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>no</Message></Error>")
			case r.Method == http.MethodPut:
				w.Header().Set("ETag", `"e"`)
			case r.Method == http.MethodDelete:
				w.WriteHeader(http.StatusForbidden)
				io.WriteString(w, "<Error><Code>AccessDenied</Code><Message>abort denied</Message></Error>")
			}
		}))
		defer srv.Close()
		sh, _ := Open("s3://bucket/p/?endpoint="+url.QueryEscape(srv.URL), Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"})
		err := sh.Put(context.Background(), id, bytes.NewReader(data), 20)
		if err == nil || !strings.Contains(err.Error(), "U1") || !strings.Contains(err.Error(), "left open") {
			t.Fatalf("a failed abort went unsaid: %v", err)
		}
	})

	t.Run("lost reply to starting", func(t *testing.T) {
		var mu sync.Mutex
		created := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodPost && r.URL.Query().Has("uploads") {
				created++
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
			}
		}))
		defer srv.Close()
		sh, _ := Open("s3://bucket/p/?endpoint="+url.QueryEscape(srv.URL), Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"})
		err := sh.Put(context.Background(), id, bytes.NewReader(data), 20)
		if err == nil || !strings.Contains(err.Error(), "AbortIncompleteMultipartUpload") {
			t.Fatalf("lost start: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		if created != 1 {
			t.Fatalf("%d uploads started for one Put", created)
		}
	})

	t.Run("panic", func(t *testing.T) {
		bucket, sh := testBucket(t)
		r := &panicReader{data: data, after: 7}
		func() {
			defer func() {
				if recover() == nil {
					t.Error("the panic was swallowed")
				}
			}()
			sh.Put(context.Background(), id, r, 20)
		}()
		if bucket.aborted != 1 || len(bucket.uploads) != 0 {
			t.Fatalf("after a panic: %d open, %d aborted", len(bucket.uploads), bucket.aborted)
		}
	})

	t.Run("cancel", func(t *testing.T) {
		bucket, sh := testBucket(t)
		ctx, cancel := context.WithCancel(context.Background())
		r := &cancelReader{data: data, cancelAt: 10, cancel: cancel}
		err := sh.Put(ctx, id, r, 20)
		if !errors.Is(err, context.Canceled) || bucket.aborted != 1 {
			t.Fatalf("cancel: %v, %d aborted", err, bucket.aborted)
		}
	})
}

type panicReader struct {
	data  []byte
	off   int
	after int
}

func (p *panicReader) Read(b []byte) (int, error) {
	if p.off >= p.after {
		panic("reader exploded")
	}
	n := copy(b, p.data[p.off:p.after])
	p.off += n
	return n, nil
}

type cancelReader struct {
	data     []byte
	off      int
	cancelAt int
	cancel   func()
}

func (c *cancelReader) Read(b []byte) (int, error) {
	if c.off >= len(c.data) {
		return 0, io.EOF
	}
	if c.off >= c.cancelAt {
		c.cancel()
	}
	n := copy(b, c.data[c.off:])
	c.off += n
	return n, nil
}

// A completion whose reply is cut off, or holds no result, is not a
// stored object, and the upload is aborted.
func TestCompletionMustBeWhole(t *testing.T) {
	defer func(p int64) { minPartSize = p }(minPartSize)
	defer func(d time.Duration) { putBackoff = d }(putBackoff)
	minPartSize, putBackoff = 5, time.Millisecond
	for name, reply := range map[string]func(w http.ResponseWriter){
		"cut off": func(w http.ResponseWriter) {
			w.Header().Set("Content-Length", "4096")
			w.WriteHeader(200)
			io.WriteString(w, "\n   \n   ")
		},
		"no result": func(w http.ResponseWriter) { io.WriteString(w, "\n   \n") },
	} {
		var aborted atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			switch {
			case r.Method == http.MethodPost && q.Has("uploads"):
				io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>U</UploadId></InitiateMultipartUploadResult>")
			case r.Method == http.MethodPut:
				w.Header().Set("ETag", `"e"`)
			case r.Method == http.MethodPost:
				reply(w)
			case r.Method == http.MethodDelete:
				aborted.Add(1)
				w.WriteHeader(204)
			}
		}))
		sh, _ := Open("s3://bucket/p/?endpoint="+url.QueryEscape(srv.URL), Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"})
		data := []byte("0123456789abcdefghij")
		err := sh.Put(context.Background(), c4.Identify(bytes.NewReader(data)), bytes.NewReader(data), 20)
		srv.Close()
		if err == nil || aborted.Load() != 1 {
			t.Errorf("%s: %v, %d aborted", name, err, aborted.Load())
		}
	}
}

// A part that is retried is sent again with the same bytes and hash.
func TestRetriedPartIsTheSamePart(t *testing.T) {
	defer func(p int64) { minPartSize = p }(minPartSize)
	defer func(d time.Duration) { putBackoff = d }(putBackoff)
	minPartSize, putBackoff = 5, time.Millisecond
	var mu sync.Mutex
	seen := map[string][]string{}
	fail := map[string]int{"2": 2}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		q := r.URL.Query()
		body, _ := io.ReadAll(r.Body)
		if sum := sha256.Sum256(body); r.Header.Get("X-Amz-Content-Sha256") != hex.EncodeToString(sum[:]) {
			w.WriteHeader(400)
			return
		}
		switch {
		case r.Method == http.MethodPost && q.Has("uploads"):
			io.WriteString(w, "<InitiateMultipartUploadResult><UploadId>U</UploadId></InitiateMultipartUploadResult>")
		case r.Method == http.MethodPut:
			pn := q.Get("partNumber")
			seen[pn] = append(seen[pn], string(body))
			if fail[pn] > 0 {
				fail[pn]--
				w.WriteHeader(503)
				return
			}
			w.Header().Set("ETag", `"e`+pn+`"`)
		case r.Method == http.MethodPost:
			io.WriteString(w, "<CompleteMultipartUploadResult/>")
		}
	}))
	defer srv.Close()
	sh, _ := Open("s3://bucket/p/?endpoint="+url.QueryEscape(srv.URL), Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"})
	data := []byte("0123456789abcdefghij")
	if err := sh.Put(context.Background(), c4.Identify(bytes.NewReader(data)), bytes.NewReader(data), 20); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(seen["2"], ","); got != "56789,56789,56789" {
		t.Fatalf("part 2 sent as %s", got)
	}
}

// However Credentials is formatted or logged, its secret and token don't
// show.
func TestCredentialsNeverPrintTheirSecrets(t *testing.T) {
	c := Credentials{AccessKeyID: "AK", SecretAccessKey: "SECRETVAL", SessionToken: "TOKVAL", Expires: time.Unix(1800000000, 0)}
	for _, s := range []string{fmt.Sprint(c), fmt.Sprintf("%v %+v %#v %s", c, c, c, c), fmt.Sprintf("%v", &c), c.LogValue().String()} {
		if strings.Contains(s, "SECRETVAL") || strings.Contains(s, "TOKVAL") || !strings.Contains(s, "AK") {
			t.Errorf("printed as %s", s)
		}
	}
}

// STS keys with no expiration are refused, without quoting them.
func TestMissingExpirationIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `<R><AssumeRoleWithWebIdentityResult><Credentials><AccessKeyId>ASIAX</AccessKeyId><SecretAccessKey>SECRETVAL</SecretAccessKey><SessionToken>TOKVAL</SessionToken></Credentials></AssumeRoleWithWebIdentityResult></R>`)
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("jwt"), 0o600)
	c := &Chain{Getenv: env(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": tok, "AWS_ROLE_ARN": "arn:aws:iam::1:role/r"}), STSEndpoint: srv.URL}
	_, err := c.Retrieve(context.Background())
	if err == nil || strings.Contains(err.Error(), "SECRETVAL") || strings.Contains(err.Error(), "TOKVAL") {
		t.Fatalf("missing expiration: %v", err)
	}
}

// A region becomes part of a host name, so it must be a region name.
func TestRegionMustBeARegionName(t *testing.T) {
	creds := Credentials{AccessKeyID: "AK", SecretAccessKey: "SK"}
	for _, r := range []string{"evil.example/x", "us-east-1@evil", "US-EAST-1", "a b"} {
		if _, err := Open("s3://bucket/?region="+url.QueryEscape(r), creds); err == nil {
			t.Errorf("region %q accepted", r)
		}
	}
	if _, err := Open("s3://bucket/?region=eu-west-3", creds); err != nil {
		t.Error(err)
	}
	c := &Chain{Getenv: env(map[string]string{"AWS_WEB_IDENTITY_TOKEN_FILE": "/x", "AWS_ROLE_ARN": "r", "AWS_REGION": "evil.example/"})}
	if _, err := c.stsEndpoint(); err == nil {
		t.Error("AWS_REGION not checked")
	}
}

// Without --aws-instance-role the chain never asks the metadata service:
// an importer whose Secret is missing fails rather than writing as the
// machine.
func TestInstanceRoleOnlyWhenAsked(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked.Add(1) }))
	defer srv.Close()
	c := &Chain{Getenv: env(nil), IMDSEndpoint: srv.URL}
	if _, err := c.Retrieve(context.Background()); err == nil || asked.Load() != 0 {
		t.Fatalf("without the flag: %v, metadata asked %d times", err, asked.Load())
	}
}
