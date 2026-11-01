package farmbook

import (
	"crypto/x509"
	"encoding/json"
	"fmt"
)

// Page is the book as GET /api/v1/book/farm?since= serves it
// (internal/hud/API.md): the signed segments after since, the entries they
// seal, and the entries pending the seal. A client checks it with
// CheckPage against the last head it kept.
type Page struct {
	Farm  string `json:"farm"`
	Since int64  `json:"since"`
	// Head is the book's sealed head now.
	Head Head `json:"head"`
	// Segments are the signed segments (book/v1 bundles) sealing positions
	// after Since, oldest first, and always the last one the page reaches,
	// so a client whose head is current still gets it to compare.
	Segments []json.RawMessage `json:"segments"`
	// Entries are the sealed entries after Since, through the last
	// segment's last position.
	Entries []PageEntry `json:"entries"`
	// Pending are the entries waiting for the seal, at provisional
	// positions: what the HUD judges with, and what no verifier counts.
	Pending []PageEntry `json:"pending"`
	// More says the page stopped at a segment before the head: read again
	// from Next.
	More bool  `json:"more,omitempty"`
	Next int64 `json:"next"`
}

// PageEntry is one entry as stored, or redacted: Redacted names the fields
// left out (10.4 item 19: ID tokens and addresses go only to their person
// and the farm's admins), and such an entry is checked by its ID alone.
type PageEntry struct {
	ID       string          `json:"id"`
	Position int64           `json:"position"`
	Segment  int64           `json:"segment,omitempty"`
	Entry    json.RawMessage `json:"entry"`
	Redacted []string        `json:"redacted,omitempty"`
}

// NewPage is the page of view after since: whole segments, as many as hold
// about limit entries (at least one), then the pending entries.
func NewPage(v *View, since int64, limit int) *Page {
	p := &Page{Farm: v.Farm, Since: since, Head: v.Head(), Segments: []json.RawMessage{}, Entries: []PageEntry{}, Pending: []PageEntry{}}
	p.Next = max(since, 0)
	n := 0
	for _, s := range v.Segments(since) {
		if n >= limit && len(p.Segments) > 0 {
			p.More = true
			break
		}
		p.Segments = append(p.Segments, json.RawMessage(s.Signed))
		for k, id := range s.Entries {
			pos := s.First + int64(k)
			if pos <= since {
				continue
			}
			p.Entries = append(p.Entries, PageEntry{ID: id, Position: pos, Segment: s.Number, Entry: v.entries[pos-1].Raw})
			n++
		}
		p.Next = max(p.Next, s.Last)
	}
	if !p.More {
		for _, s := range v.pending {
			p.Pending = append(p.Pending, PageEntry{ID: s.ID, Position: s.Entry.Position, Entry: s.Raw})
		}
	}
	return p
}

// CheckPage checks a page a client read with since set to its last head's
// position, and returns the head to keep next: the page's last segment.
// Every segment verifies, is the queue's and this farm's, and follows the
// last head by number and link; every entry is listed by its segment at
// its position, and one not redacted hashes to its ID and names the entry
// before it. A head older than last, or another segment in its place, is
// ErrRollback: that is what stops a rollback, since an older head the
// queue signed verifies by itself.
func CheckPage(farm string, roots *x509.CertPool, last LastHead, p *Page) (LastHead, error) {
	if err := last.After(p.Head); err != nil {
		return last, err
	}
	if p.Farm != farm {
		return last, fmt.Errorf("%w: the page is farm %.16s's", ErrBook, p.Farm)
	}
	if p.Since != last.Position {
		return last, fmt.Errorf("read from since=%d, the last head's position, to check the page against it", last.Position)
	}
	var prev *Segment
	lastEntry := last.Entry
	if last.Number > 0 {
		prev = &Segment{ID: last.Segment}
		prev.Number, prev.Last = last.Number, last.Position
	}
	var segs []*Segment
	for k, raw := range p.Segments {
		id := c4id(raw)
		if k == 0 && last.Number > 0 && id == last.Segment {
			continue // the head the client has, when nothing is new
		}
		s, err := checkSegment(farm, roots, id, raw, prev, lastEntry)
		if err != nil {
			if prev != nil && k == 0 {
				return last, fmt.Errorf("%w: %v", ErrRollback, err)
			}
			return last, err
		}
		segs = append(segs, s)
		prev = s
		if n := len(s.Entries); n > 0 {
			lastEntry = s.Entries[n-1]
		}
	}
	next := last
	if prev != nil {
		next = LastHead{Number: prev.Number, Segment: prev.ID, Position: prev.Last, Entry: lastEntry}
	}
	if !p.More && (next.Number != p.Head.Number || next.Segment != p.Head.Segment) {
		return last, fmt.Errorf("%w: the page ends at segment %d, the head it names is %d", ErrBook, next.Number, p.Head.Number)
	}
	listed := map[int64]string{}
	for _, s := range segs {
		for k, id := range s.Entries {
			listed[s.First+int64(k)] = id
		}
	}
	before := last.Entry
	for k, e := range p.Entries {
		if e.Position != last.Position+int64(k)+1 || listed[e.Position] != e.ID {
			return last, fmt.Errorf("%w: entry %.16s at %d isn't the one its segment seals there", ErrBook, e.ID, e.Position)
		}
		if len(e.Redacted) == 0 {
			if _, err := checkEntry(farm, e.ID, e.Entry, e.Position, before, segs[0].Adopted != nil && segs[0].Number == 1 && e.Position <= segs[0].Last); err != nil {
				return last, err
			}
		}
		before = e.ID
	}
	if int64(len(p.Entries)) != next.Position-last.Position {
		return last, fmt.Errorf("%w: the page holds %d entries, its segments seal %d", ErrBook, len(p.Entries), next.Position-last.Position)
	}
	return next, nil
}
