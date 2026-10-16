package cache

import (
	"os"
	"sort"
	"time"

	"github.com/Avalanche-io/c4"
)

// EvictStats reports one eviction pass.
type EvictStats struct {
	Objects     int
	ObjectBytes int64
	Views       int
	Over        int64 // bytes still over budget afterwards (0 when within)
}

// Evict brings the cache under its budget.
//
// Objects no view links (link count 1) go first, least recently used
// first. If that is not enough, views that no pod on the node is using
// and whose retention has passed are removed, least recently used first,
// which frees their objects for the next round. Views in use or still
// retained are never touched; if they alone exceed the budget the cache
// stays over it and says so.
func (c *Cache) Evict(inUse map[c4.ID]bool, now time.Time) (EvictStats, error) {
	c.gc.Lock()
	defer c.gc.Unlock()

	var st EvictStats
	if c.Bytes() <= c.budget {
		return st, nil
	}
	if err := c.evictUnlinked(&st); err != nil {
		return st, err
	}

	views, err := c.Views()
	if err != nil {
		return st, err
	}
	for _, v := range views {
		if c.Bytes() <= c.budget {
			break
		}
		if inUse[v.ID] || now.Before(v.Meta.LastUsed.Add(v.Meta.Retain)) {
			continue
		}
		if err := c.RemoveView(v.ID); err != nil {
			continue
		}
		c.log.Info("evicted view", "view", v.ID.String(), "lastUsed", v.Meta.LastUsed)
		st.Views++
		if err := c.evictUnlinked(&st); err != nil {
			return st, err
		}
	}
	if over := c.Bytes() - c.budget; over > 0 {
		st.Over = over
	}
	return st, nil
}

type candidate struct {
	id    c4.ID
	size  int64
	atime time.Time
}

func (c *Cache) evictUnlinked(st *EvictStats) error {
	var cands []candidate
	err := c.objects.Walk(func(id c4.ID, size int64) error {
		p, ok := c.objects.ContentPath(id)
		if !ok {
			return nil
		}
		nlink, atime, err := linkInfo(p)
		if err != nil || nlink > 1 {
			return nil
		}
		cands = append(cands, candidate{id, size, atime})
		return nil
	})
	if err != nil {
		return err
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].atime.Before(cands[j].atime) })
	for _, cd := range cands {
		if c.Bytes() <= c.budget {
			return nil
		}
		p, ok := c.objects.ContentPath(cd.id)
		if !ok {
			continue
		}
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		c.remove(cd.id, p)
		st.Objects++
		st.ObjectBytes += cd.size
	}
	return nil
}
