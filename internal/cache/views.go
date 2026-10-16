package cache

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4-operator/internal/manifest"
)

// ViewMeta is what the node remembers about a view besides its tree.
type ViewMeta struct {
	Materialization string        `json:"materialization"` // namespace/name that last used it
	Retain          time.Duration `json:"retain"`          // pinned this long after last use
	LastUsed        time.Time     `json:"lastUsed"`
}

func (c *Cache) metaPath(id c4.ID, ext string) string {
	return filepath.Join(c.root, "meta", id.String()+ext)
}

// LoadViewDesc returns a cached view description, rechecked against id.
func (c *Cache) LoadViewDesc(id c4.ID) (*manifest.View, error) {
	data, err := os.ReadFile(c.metaPath(id, ".c4m"))
	if err != nil {
		return nil, err
	}
	return manifest.ParseView(data, id)
}

// SaveViewDesc records a view's description so later pods on this node
// need neither the API nor the store to know what the view holds.
func (c *Cache) SaveViewDesc(v *manifest.View) error {
	data, err := v.Marshal()
	if err != nil {
		return err
	}
	return writeAtomic(c.metaPath(v.ID, ".c4m"), data)
}

// TouchView records use of a view.
func (c *Cache) TouchView(id c4.ID, m ViewMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeAtomic(c.metaPath(id, ".json"), data)
}

// ViewInfo describes a materialized view.
type ViewInfo struct {
	ID   c4.ID
	Meta ViewMeta
}

// Views lists materialized views, least recently used first.
func (c *Cache) Views() ([]ViewInfo, error) {
	ents, err := os.ReadDir(filepath.Join(c.root, "views"))
	if err != nil {
		return nil, err
	}
	var out []ViewInfo
	for _, e := range ents {
		if !e.IsDir() || !isViewName(e.Name()) {
			continue
		}
		id, err := c4.Parse(e.Name())
		if err != nil {
			continue
		}
		vi := ViewInfo{ID: id}
		if data, err := os.ReadFile(c.metaPath(id, ".json")); err == nil {
			json.Unmarshal(data, &vi.Meta)
		}
		out = append(out, vi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.LastUsed.Before(out[j].Meta.LastUsed) })
	return out, nil
}

// RemoveView deletes a materialized view. Its objects stay cached; with
// the view's links gone they become evictable.
func (c *Cache) RemoveView(id c4.ID) error {
	dir := c.ViewDir(id)
	old := filepath.Join(c.root, "views", ".old-"+id.String())
	if err := os.Rename(dir, old); err != nil {
		return err
	}
	os.Remove(c.metaPath(id, ".json"))
	return os.RemoveAll(old)
}

func writeAtomic(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}
