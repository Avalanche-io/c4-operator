package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sort"
)

// ModelUsage is one model's token counts, in the shape of a unit's
// .c4/usage.json (c4-unit usage; the queue's model spend). CacheWriteTokens
// are the prompt-cache writes, priced apart from plain input; a reader that
// does not know them ignores them.
type ModelUsage struct {
	Model            string   `json:"model"`
	InputTokens      int64    `json:"inputTokens"`
	OutputTokens     int64    `json:"outputTokens"`
	CacheReadTokens  int64    `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64    `json:"cacheWriteTokens,omitempty"`
	CostUSD          *float64 `json:"costUSD,omitempty"`
}

// UsageDoc is a unit's .c4/usage.json.
type UsageDoc struct {
	Models     []ModelUsage `json:"models"`
	CostSource string       `json:"costSource,omitempty"`
}

// TranscriptUsage totals the token use a Claude Code transcript records
// between byte offsets from and to (to <= 0: its end), per model. A
// transcript writes one API message as several lines, each carrying the
// message's usage, so each message ID counts once. A session's own count is
// tokens, not dollars: it sets no cost.
func TranscriptUsage(path string, from, to int64) (*UsageDoc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, err
	}
	var r io.Reader = f
	if to > 0 {
		r = io.LimitReader(f, max(to-from, 0))
	}
	type line struct {
		Type    string `json:"type"`
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage *struct {
				Input      int64 `json:"input_tokens"`
				Output     int64 `json:"output_tokens"`
				CacheRead  int64 `json:"cache_read_input_tokens"`
				CacheWrite int64 `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
	}
	seen := map[string]bool{}
	by := map[string]*ModelUsage{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil || l.Type != "assistant" || l.Message.Usage == nil || l.Message.Model == "" {
			continue
		}
		if id := l.Message.ID; id != "" {
			if seen[id] {
				continue
			}
			seen[id] = true
		}
		m := by[l.Message.Model]
		if m == nil {
			m = &ModelUsage{Model: l.Message.Model}
			by[l.Message.Model] = m
		}
		u := l.Message.Usage
		m.InputTokens += u.Input
		m.OutputTokens += u.Output
		m.CacheReadTokens += u.CacheRead
		m.CacheWriteTokens += u.CacheWrite
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	d := &UsageDoc{Models: []ModelUsage{}}
	for _, m := range by {
		d.Models = append(d.Models, *m)
	}
	sort.Slice(d.Models, func(i, j int) bool { return d.Models[i].Model < d.Models[j].Model })
	return d, nil
}

// AttemptStart is where this attempt's turns begin in the member's
// transcript: the offset of the checkpoint before cp in the same session
// (the one the attempt resumed from), or 0 when cp starts its session's
// chain.
func (s *Store) AttemptStart(name string, cp *Checkpoint) int64 {
	cps, err := s.readLog(name)
	if err != nil || cp == nil {
		return 0
	}
	var from int64
	for _, c := range cps {
		if c.Session == cp.Session && c.Seq < cp.Seq {
			from = c.Offset
		}
	}
	return from
}
