package farmcli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Avalanche-io/c4"
)

// SamplesBase is where releases publish the sample sets.
var SamplesBase = "https://github.com/Avalanche-io/c4-operator/releases/download"

// SampleSets are the sets `c4 farm samples get` knows; every set comes in
// one archive, c4-samples-VERSION.tar.gz, with its C4 ID beside it in
// c4-samples-VERSION.tar.gz.c4.
var SampleSets = []string{"saltmarsh"}

// SamplesArgs are `c4 farm samples get`'s arguments.
type SamplesArgs struct {
	Set     string
	Version string
	Dir     string // default ~/c4-samples
	From    string // a local archive instead of the release's
	ID      string // the archive's expected C4 ID (default the release's .c4 file)
	HTTP    *http.Client
}

// GetSamples downloads (or reads) the sample archive, checks its C4 ID,
// and unpacks it under Dir.
func GetSamples(ctx context.Context, o Out, a SamplesArgs) error {
	if a.Set == "" {
		a.Set = "saltmarsh"
	}
	known := false
	for _, s := range SampleSets {
		known = known || s == a.Set
	}
	if !known {
		return fmt.Errorf("no sample set %q; there is: %s", a.Set, strings.Join(SampleSets, ", "))
	}
	if a.Dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		a.Dir = filepath.Join(home, "c4-samples")
	}
	hc := a.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	tmp, err := os.CreateTemp("", "c4-samples-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	want := a.ID
	if a.From != "" {
		f, err := os.Open(a.From)
		if err != nil {
			return err
		}
		_, err = io.Copy(tmp, f)
		f.Close()
		if err != nil {
			return err
		}
	} else {
		version, dev, err := ReleaseFor(ctx, hc, a.Version)
		if errors.Is(err, ErrNoRelease) {
			return errors.New("this is a development build and no release is published yet: pass --from ARCHIVE (hack/samples/build.sh makes one)")
		}
		if err != nil {
			return err
		}
		if dev {
			o.printf("a development build: the samples of the latest release, %s\n", version)
		}
		name := "c4-samples-" + version + ".tar.gz"
		if want == "" {
			b, err := download(ctx, hc, SamplesBase+"/"+version+"/"+name+".c4", io.Discard, 1<<10)
			if errors.Is(err, errNoAsset) {
				return fmt.Errorf("release %s has no sample set attached yet: c4 farm samples load works without it, and --from ARCHIVE takes a set you have", version)
			}
			if err != nil {
				return err
			}
			want = strings.TrimSpace(string(b))
		}
		o.printf("downloading %s\n", name)
		if _, err := download(ctx, hc, SamplesBase+"/"+version+"/"+name, tmp, 0); err != nil {
			return err
		}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	got := c4.Identify(tmp)
	if want != "" {
		id, err := c4.Parse(want)
		if err != nil {
			return fmt.Errorf("the published C4 ID %q is not one", want)
		}
		if got != id {
			return fmt.Errorf("the archive's C4 ID is %s, not the published %s: not unpacked", got, id)
		}
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return err
	}
	n, size, err := untar(tmp, a.Dir)
	if err != nil {
		return err
	}
	o.printf("%s · %s in %s\n", plural(n, "file", "files"), Bytes(size), a.Dir)
	o.printf("archive   %s%s\n", ShortID(got.String()), map[bool]string{true: " · checked", false: ""}[want != ""])
	return nil
}

// download GETs a URL into w (or, with limit > 0, returns up to limit bytes).
// errNoAsset is a release with nothing of that name attached.
var errNoAsset = errors.New("404 Not Found")

func download(ctx context.Context, hc *http.Client, url string, w io.Writer, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("downloading %s: %w", url, errNoAsset)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	if limit > 0 {
		return io.ReadAll(io.LimitReader(resp.Body, limit))
	}
	_, err = io.Copy(w, resp.Body)
	return nil, err
}

// untar unpacks regular files and folders under dir, refusing any path
// that would land outside it, and links of any kind.
func untar(r io.Reader, dir string) (int, int64, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, 0, err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	n, size := 0, int64(0)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return n, size, nil
		}
		if err != nil {
			return n, size, err
		}
		rel := filepath.FromSlash(strings.TrimPrefix(h.Name, "./"))
		if rel == "" || rel == "." {
			continue
		}
		if !filepath.IsLocal(rel) {
			return n, size, fmt.Errorf("the archive has a path outside its folder: %s", h.Name)
		}
		dst := filepath.Join(dir, rel)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return n, size, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return n, size, err
			}
			mode := os.FileMode(0o644)
			if h.Mode&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return n, size, err
			}
			w, err := io.Copy(f, tr)
			f.Close()
			if err != nil {
				return n, size, err
			}
			n++
			size += w
		case tar.TypeXGlobalHeader:
		default:
			return n, size, fmt.Errorf("the archive has a link or device: %s", h.Name)
		}
	}
}
