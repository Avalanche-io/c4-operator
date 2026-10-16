package storeurl

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4"
)

// The "GET Object" example of the AWS Signature Version 4 documentation
// (Authenticating Requests: Using the Authorization Header).
func TestSignV4MatchesAWSExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	creds := Credentials{AccessKeyID: "AKIAIOSFODNN7EXAMPLE", SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	when := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	signV4Payload(req, creds, "us-east-1", when, "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855")
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

func TestTreeShardResumesAtOffset(t *testing.T) {
	sh, err := Open(t.TempDir(), Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("0123456789abcdef")
	id := c4.Identify(bytes.NewReader(data))
	if err := sh.Put(context.Background(), id, bytes.NewReader(data), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	body, total, start, err := sh.Get(context.Background(), id, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	rest, _ := io.ReadAll(body)
	if total != 16 || start != 10 || string(rest) != "abcdef" {
		t.Fatalf("total %d start %d rest %q", total, start, rest)
	}
	if _, err := sh.Head(context.Background(), c4.Identify(strings.NewReader("absent"))); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent object: %v", err)
	}
}

func TestShardedSpreadsAndIsStable(t *testing.T) {
	dirs := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	s, err := OpenSharded(dirs, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for i := range 1200 {
		data := []byte{byte(i), byte(i >> 8), 1}
		id, err := s.PutBytes(context.Background(), data)
		if err != nil {
			t.Fatal(err)
		}
		counts[s.For(id).Name()]++
		if _, err := s.Head(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	for name, n := range counts {
		if n < 320 || n > 480 {
			t.Errorf("shard %s holds %d of 1200", name, n)
		}
	}
	if _, err := OpenSharded([]string{dirs[0], dirs[0]}, Credentials{}); err == nil {
		t.Fatal("a shard listed twice was accepted")
	}
}
