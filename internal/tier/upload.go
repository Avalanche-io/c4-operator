package tier

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"
)

// Uploader writes objects through the regional tier with PUT, so the
// writer never chooses the key: the tier hashes the body and refuses it if
// it does not match.
type Uploader struct {
	// Candidates returns regional replicas for (project, id), best first.
	Candidates func(project string, id c4.ID) []*HTTPSource
	Breakers   *Breakers
	Backoff    Backoff
	Client     *http.Client
	Rounds     int // retry rounds over all candidates (0: 8)
}

// PutFile uploads a file whose ID the caller computed.
func (u *Uploader) PutFile(ctx context.Context, project string, id c4.ID, path string) error {
	return u.put(ctx, project, id, func() (io.ReadCloser, int64, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, 0, err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return nil, 0, err
		}
		return f, st.Size(), nil
	})
}

// PutBytes uploads small content (records, changesets).
func (u *Uploader) PutBytes(ctx context.Context, project string, id c4.ID, data []byte) error {
	return u.put(ctx, project, id, func() (io.ReadCloser, int64, error) {
		return io.NopCloser(strings.NewReader(string(data))), int64(len(data)), nil
	})
}

func (u *Uploader) put(ctx context.Context, project string, id c4.ID, open func() (io.ReadCloser, int64, error)) error {
	rounds := u.Rounds
	if rounds <= 0 {
		rounds = 8
	}
	var last error
	for round := range rounds {
		for _, c := range u.Candidates(project, id) {
			if !u.Breakers.Allow(c.Name()) {
				continue
			}
			err := u.putOnce(ctx, c, project, id, open)
			if err == nil {
				u.Breakers.Ok(c.Name())
				return nil
			}
			last = err
			if errors.Is(err, errRejected) {
				return err
			}
			if !errors.Is(err, ErrBusy) {
				u.Breakers.Fail(c.Name())
			}
		}
		select {
		case <-time.After(u.Backoff.Delay(round)):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if last == nil {
		last = ErrNoRoute
	}
	return fmt.Errorf("upload %s: %w", id, last)
}

var errRejected = errors.New("rejected by the tier")

func (u *Uploader) putOnce(ctx context.Context, c *HTTPSource, project string, id c4.ID, open func() (io.ReadCloser, int64, error)) error {
	body, size, err := open()
	if err != nil {
		return err
	}
	defer body.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.Base+"/", body)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set(HeaderProject, project)
	req.Header.Set(HeaderID, id.String())
	req.Header.Set("Content-Type", "application/octet-stream")
	if c.Token != nil {
		if t := c.Token(); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	client := u.Client
	if client == nil {
		client = c.Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	switch {
	case resp.StatusCode == http.StatusOK:
		if got := strings.TrimSpace(string(msg)); got != id.String() {
			return fmt.Errorf("%w: tier stored %s as %s", errRejected, id, got)
		}
		return nil
	case resp.StatusCode == http.StatusServiceUnavailable:
		return ErrBusy
	case resp.StatusCode == http.StatusUnprocessableEntity, resp.StatusCode == http.StatusBadRequest:
		return fmt.Errorf("%w: %s", errRejected, strings.TrimSpace(string(msg)))
	}
	return fmt.Errorf("PUT %s at %s: status %d: %s", id, c.Name(), resp.StatusCode, strings.TrimSpace(string(msg)))
}

// TokenFile reads a projected service-account token from disk, rereading
// it when kubelet rotates it.
type TokenFile struct {
	Path string

	mu    sync.Mutex
	token string
	read  time.Time
}

// Token returns the current token ("" if unreadable).
func (t *TokenFile) Token() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" && time.Since(t.read) < time.Minute {
		return t.token
	}
	b, err := os.ReadFile(t.Path)
	if err != nil {
		return t.token
	}
	t.token = strings.TrimSpace(string(b))
	t.read = time.Now()
	return t.token
}
