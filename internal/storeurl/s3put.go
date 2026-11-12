package storeurl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Avalanche-io/c4"
)

// Objects up to one part go up in a single PUT; larger ones as a multipart
// upload. Each part is held in memory once, so a part that fails is sent
// again without spooling the object to disk. A part is at least 8 MiB and
// grows with a known size, so S3's 10,000-part limit never caps an object.
var minPartSize int64 = 8 << 20

const maxParts = 10000

func partSizeFor(size int64) int64 {
	p := minPartSize
	if size > 0 {
		if n := (size + maxParts - 1) / maxParts; n > p {
			p = (n + 1<<20 - 1) &^ (1<<20 - 1)
		}
	}
	return p
}

// Put writes size bytes from r under id (size < 0: unknown). It hashes
// what it sends and publishes only bytes that are id's: a single PUT is
// sent only once its bytes hash to id, and a multipart upload, whose parts
// are invisible until it completes, is completed only then, and aborted
// otherwise (ErrNotItsID). An S3 PUT replaces whatever the key holds; what
// it replaces is id's bytes again, or other bytes, which no read takes as
// id's.
func (s *s3Shard) Put(ctx context.Context, id c4.ID, r io.Reader, size int64) error {
	key := s.prefix + id.String()
	part := partSizeFor(size)
	// A known size small enough for one PUT allocates only that, plus one
	// byte to see that the reader ends where it said.
	first0 := part
	if size >= 0 {
		first0 = min(part, size+1)
	}
	first, err := readPart(r, first0)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", id, err)
	}
	h := sha512.New()
	if int64(len(first)) < first0 || first0 < part {
		h.Write(first)
		if size >= 0 && int64(len(first)) != size {
			return fmt.Errorf("s3 put %s: read %d bytes, expected %d", id, len(first), size)
		}
		if err := sameID(id, h); err != nil {
			return fmt.Errorf("s3 put %w", err)
		}
		resp, err := s.send(ctx, http.MethodPut, key, "", first, true)
		if err != nil {
			return fmt.Errorf("s3 put %s: %w", id, err)
		}
		resp.Body.Close()
		return nil
	}
	return s.multipart(ctx, id, key, first, h, r, part, size)
}

// sameID says whether what h has hashed is id's bytes.
func sameID(id c4.ID, h hash.Hash) error {
	var got c4.ID
	copy(got[:], h.Sum(nil))
	if got != id {
		return fmt.Errorf("%s: %w (they are %s)", id, ErrNotItsID, got)
	}
	return nil
}

func readPart(r io.Reader, n int64) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	return buf[:got], err
}

func (s *s3Shard) multipart(ctx context.Context, id c4.ID, key string, first []byte, h hash.Hash, r io.Reader, part, size int64) (err error) {
	// Creating an upload isn't idempotent: if the reply is lost, a retry
	// would make a second upload and leave the first open, billed. So a
	// transport failure here is not retried.
	resp, err := s.send(ctx, http.MethodPost, key, "uploads=", nil, false)
	if err != nil {
		return fmt.Errorf("s3 put %s: start upload: %w (if S3 created it before the reply was lost, its ID is unknown here; a bucket lifecycle rule, AbortIncompleteMultipartUpload, removes it)", id, err)
	}
	var started struct {
		UploadID string `xml:"UploadId"`
	}
	err = xml.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&started)
	resp.Body.Close()
	if err != nil || started.UploadID == "" {
		return fmt.Errorf("s3 put %s: start upload: no upload ID (%v)", id, err)
	}
	upload := "uploadId=" + uriEncode(started.UploadID, true)
	// An upload that doesn't complete is aborted, whatever ends it (an
	// error, a cancelled context, a panic in the reader): its parts are
	// billed until it is. An abort that fails says which upload is left.
	abort := func() error {
		actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), abortTimeout)
		defer cancel()
		resp, aerr := s.send(actx, http.MethodDelete, key, upload, nil, true)
		if aerr != nil {
			return fmt.Errorf("upload %s of %s left open (abort failed: %w); a bucket lifecycle rule (AbortIncompleteMultipartUpload) removes it", started.UploadID, key, aerr)
		}
		resp.Body.Close()
		return nil
	}
	defer func() {
		if p := recover(); p != nil {
			abort()
			panic(p)
		}
		if err != nil {
			if aerr := abort(); aerr != nil {
				err = errors.Join(err, aerr)
			}
		}
	}()

	type done struct {
		Number int    `xml:"PartNumber"`
		ETag   string `xml:"ETag"`
	}
	var parts []done
	var total int64
	buf := first
	for n := 1; len(buf) > 0; n++ {
		if n > maxParts {
			return fmt.Errorf("s3 put %s: more than %d parts", id, maxParts)
		}
		// The part is hashed while it is sent, which hides the hash behind
		// the upload (measured on a local farm's Garage: 3 to 6% of a 64
		// MiB to 1 GiB upload, against 12 to 17% hashing first). Its bytes
		// are not changed by either, and the next part is read only after.
		hashed := make(chan struct{})
		go func(b []byte) { h.Write(b); close(hashed) }(buf)
		resp, err := s.send(ctx, http.MethodPut, key, "partNumber="+strconv.Itoa(n)+"&"+upload, buf, true)
		<-hashed
		if err != nil {
			return fmt.Errorf("s3 put %s: part %d: %w", id, n, err)
		}
		resp.Body.Close()
		etag := resp.Header.Get("ETag")
		if etag == "" {
			return fmt.Errorf("s3 put %s: part %d: no ETag", id, n)
		}
		parts = append(parts, done{n, etag})
		total += int64(len(buf))
		if int64(len(buf)) < part {
			break
		}
		if buf, err = readPart(r, part); err != nil {
			return fmt.Errorf("s3 put %s: %w", id, err)
		}
	}
	if size >= 0 && total != size {
		return fmt.Errorf("s3 put %s: read %d bytes, expected %d", id, total, size)
	}
	// The last chance to keep other bytes from id's key: an upload that is
	// never completed publishes nothing, and the deferred abort removes it.
	if err := sameID(id, h); err != nil {
		return fmt.Errorf("s3 put %w", err)
	}
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []done   `xml:"Part"`
	}{Parts: parts})
	resp, err = s.send(ctx, http.MethodPost, key, upload, body, true)
	if err != nil {
		return fmt.Errorf("s3 put %s: complete: %w", id, err)
	}
	// S3 answers 200 at once and streams the result while it joins the
	// parts: it can still report an error in the body, or the connection
	// can drop first. Only a whole result is a stored object.
	reply, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	switch {
	case rerr != nil:
		return fmt.Errorf("s3 put %s: complete: reply cut off: %w", id, rerr)
	case bytes.Contains(reply, []byte("<Error>")):
		return fmt.Errorf("s3 put %s: complete failed%s", id, s3Message(reply))
	case !bytes.Contains(reply, []byte("<CompleteMultipartUploadResult")):
		return fmt.Errorf("s3 put %s: complete: no result in the reply", id)
	}
	return nil
}

// Transient failures (a dropped connection, 500, 503 SlowDown) are tried
// again with backoff, re-signed each time with the current credentials.
var putRetries, putBackoff = 4, 200 * time.Millisecond

// An abort gives up after this long.
var abortTimeout = 30 * time.Second

// send makes one S3 request with body (sent, and signed, as given) and
// returns a 2xx response. retryTransport says whether a request whose
// reply never came may be sent again: not when doing it twice differs from
// doing it once. An error names S3's code and message, never its body:
// an S3 error body can quote the signed request, session token included.
func (s *s3Shard) send(ctx context.Context, method, key, query string, body []byte, retryTransport bool) (*http.Response, error) {
	u := s.keyURL(key)
	if query != "" {
		u += "?" + query
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	wait := putBackoff
	var last error
	for attempt := 0; attempt <= putRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(float64(wait) * (0.75 + rand.Float64()/2))):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			wait *= 2
		}
		req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.ContentLength = int64(len(body))
		creds, err := s.creds.Retrieve(ctx)
		if err != nil {
			return nil, err
		}
		signV4Payload(req, creds, s.region, time.Now().UTC(), hash)
		resp, err := s.client.Do(req)
		if err != nil {
			last = err
			if !retryTransport {
				return nil, err
			}
			continue
		}
		if resp.StatusCode/100 == 2 {
			return resp, nil
		}
		reply, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		last = fmt.Errorf("status %d%s", resp.StatusCode, s3Message(reply))
		if resp.StatusCode != http.StatusInternalServerError && resp.StatusCode != http.StatusServiceUnavailable {
			return nil, last
		}
	}
	return nil, errors.Join(errors.New("gave up after retries"), last)
}

// s3Message gives ": Code: Message" from an S3 error body, or "" when it
// has none. It reads those two elements only, from a whole or a cut-off
// body, and never returns anything else of it.
func s3Message(body []byte) string {
	field := func(name string) string {
		_, rest, ok := bytes.Cut(body, []byte("<"+name+">"))
		if !ok {
			return ""
		}
		v, _, ok := bytes.Cut(rest, []byte("</"+name+">"))
		if !ok || len(v) > 512 {
			return ""
		}
		return strings.TrimSpace(string(v))
	}
	code, msg := field("Code"), field("Message")
	switch {
	case code != "" && msg != "":
		return ": " + code + ": " + msg
	case code != "":
		return ": " + code
	}
	return ""
}
