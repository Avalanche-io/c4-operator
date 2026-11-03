package storeurl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
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

// Put writes size bytes from r under id (size < 0: unknown). The caller
// computed id; only trusted ingest points call Put.
func (s *s3Shard) Put(ctx context.Context, id c4.ID, r io.Reader, size int64) error {
	key := s.prefix + id.String()
	part := partSizeFor(size)
	first, err := readPart(r, part)
	if err != nil {
		return fmt.Errorf("s3 put %s: %w", id, err)
	}
	if int64(len(first)) < part {
		if size >= 0 && int64(len(first)) != size {
			return fmt.Errorf("s3 put %s: read %d bytes, expected %d", id, len(first), size)
		}
		resp, err := s.send(ctx, http.MethodPut, key, "", first)
		if err != nil {
			return fmt.Errorf("s3 put %s: %w", id, err)
		}
		resp.Body.Close()
		return nil
	}
	return s.multipart(ctx, id, key, first, r, part, size)
}

func readPart(r io.Reader, n int64) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = nil
	}
	return buf[:got], err
}

func (s *s3Shard) multipart(ctx context.Context, id c4.ID, key string, first []byte, r io.Reader, part, size int64) (err error) {
	resp, err := s.send(ctx, http.MethodPost, key, "uploads=", nil)
	if err != nil {
		return fmt.Errorf("s3 put %s: start upload: %w", id, err)
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
	defer func() {
		if err != nil {
			// Abandoned parts are billed until aborted.
			if resp, aerr := s.send(context.WithoutCancel(ctx), http.MethodDelete, key, upload, nil); aerr == nil {
				resp.Body.Close()
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
		resp, err := s.send(ctx, http.MethodPut, key, "partNumber="+strconv.Itoa(n)+"&"+upload, buf)
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
	body, _ := xml.Marshal(struct {
		XMLName xml.Name `xml:"CompleteMultipartUpload"`
		Parts   []done   `xml:"Part"`
	}{Parts: parts})
	resp, err = s.send(ctx, http.MethodPost, key, upload, body)
	if err != nil {
		return fmt.Errorf("s3 put %s: complete: %w", id, err)
	}
	// S3 can answer 200 and still report an error in the body.
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if bytes.Contains(reply, []byte("<Error>")) {
		return fmt.Errorf("s3 put %s: complete: %s", id, s3Message(reply))
	}
	return nil
}

// Transient failures (a dropped connection, 500, 503 SlowDown) are tried
// again with backoff, re-signed each time with the current credentials.
var putRetries, putBackoff = 4, 200 * time.Millisecond

// send makes one S3 request with body (sent, and signed, as given) and
// returns a 2xx response.
func (s *s3Shard) send(ctx context.Context, method, key, query string, body []byte) (*http.Response, error) {
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
			continue
		}
		if resp.StatusCode/100 == 2 {
			return resp, nil
		}
		reply, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		last = fmt.Errorf("status %d: %s", resp.StatusCode, s3Message(reply))
		if resp.StatusCode != http.StatusInternalServerError && resp.StatusCode != http.StatusServiceUnavailable {
			return nil, last
		}
	}
	return nil, errors.Join(errors.New("gave up after retries"), last)
}

func s3Message(body []byte) string {
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	if xml.Unmarshal(body, &e) == nil && e.Code != "" {
		return e.Code + ": " + e.Message
	}
	return strings.TrimSpace(string(body))
}
