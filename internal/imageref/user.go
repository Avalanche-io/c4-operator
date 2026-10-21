package imageref

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// User is the user an image runs as, from its config (the Dockerfile's
// USER), "" when it names none. A unit runs as a non-root user under Pod
// Security restricted, so an image that expects root fails on its first
// write; reading this before it runs lets the farm say why.
func (z *Resolver) User(ctx context.Context, ref string) (string, error) {
	r, err := Parse(ref)
	if err != nil {
		return "", err
	}
	reference := r.Tag
	if r.Digest != "" {
		reference = r.Digest
	}
	body, err := z.fetch(ctx, r, "manifests/"+reference, accept)
	if err != nil {
		return "", err
	}
	var m struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", fmt.Errorf("%s: the registry's manifest: %w", ref, err)
	}
	if len(m.Manifests) > 0 {
		// An index: the user is the same on every platform an image is
		// built for in practice; read the first Linux one.
		pick := ""
		for _, x := range m.Manifests {
			if x.Platform.OS == "linux" || x.Platform.OS == "" {
				pick = x.Digest
				break
			}
		}
		if pick == "" {
			return "", fmt.Errorf("%s has no Linux image", ref)
		}
		if body, err = z.fetch(ctx, r, "manifests/"+pick, accept); err != nil {
			return "", err
		}
		if err := json.Unmarshal(body, &m); err != nil {
			return "", fmt.Errorf("%s: the registry's manifest: %w", ref, err)
		}
	}
	if m.Config.Digest == "" {
		return "", fmt.Errorf("%s: the manifest names no config", ref)
	}
	cfg, err := z.fetch(ctx, r, "blobs/"+m.Config.Digest, "*/*")
	if err != nil {
		return "", err
	}
	var c struct {
		Config struct {
			User string `json:"User"`
		} `json:"config"`
	}
	if err := json.Unmarshal(cfg, &c); err != nil {
		return "", fmt.Errorf("%s: the image's config: %w", ref, err)
	}
	return c.Config.User, nil
}

// RunsAsRoot says an image's user is root: none named, 0 or root.
func RunsAsRoot(user string) bool {
	u, _, _ := strings.Cut(user, ":")
	return u == "" || u == "0" || u == "root"
}

// fetch GETs a registry path for r's repository, with an anonymous pull
// token if the registry asks for one.
func (z *Resolver) fetch(ctx context.Context, r Ref, path, accepts string) ([]byte, error) {
	host := r.Domain
	if host == "docker.io" {
		host = "registry-1.docker.io"
	}
	scheme := "https"
	if z.plain(host) {
		scheme = "http"
	}
	u := scheme + "://" + host + "/v2/" + r.Repository + "/" + path
	token := ""
	for try := 0; try < 2; try++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", accepts)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := z.client().Do(req)
		if err != nil {
			return nil, fmt.Errorf("the registry %s: %w", host, err)
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusUnauthorized && token == "":
			if token, err = z.token(ctx, resp.Header.Get("WWW-Authenticate")); err != nil {
				return nil, fmt.Errorf("the registry %s: %w", host, err)
			}
			continue
		case resp.StatusCode == http.StatusOK:
			return body, nil
		case resp.StatusCode == http.StatusNotFound:
			return nil, fmt.Errorf("the registry %s has no %s %s", host, r.Repository, path)
		default:
			return nil, fmt.Errorf("the registry %s answered %s", host, resp.Status)
		}
	}
	return nil, errors.New("the registry refused the credentials it issued")
}
