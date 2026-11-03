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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/store"
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

// The S3 shard follows no redirect: the token and the body would go to
// the Location's host, and a 302 would turn a PUT into a body-less GET
// that "succeeds". (STS and the metadata service: the c4 library's tests.)
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
}

// Without --aws-instance-role the chain never asks the metadata service:
// an importer whose Secret is missing fails rather than writing as the
// machine.
func TestInstanceRoleOnlyWhenAsked(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked.Add(1) }))
	defer srv.Close()
	c := &store.Chain{Getenv: env(nil), IMDSEndpoint: srv.URL}
	if _, err := c.Retrieve(context.Background()); err == nil || asked.Load() != 0 {
		t.Fatalf("without the flag: %v, metadata asked %d times", err, asked.Load())
	}
}
