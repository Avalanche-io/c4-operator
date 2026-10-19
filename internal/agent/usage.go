package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
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
	// Reasoning is what the attempt reasoned with, as observed.
	Reasoning *Reasoning `json:"reasoning,omitempty"`
}

// Reasoning is what an attempt reasoned with, as its session's transcript
// records it: the observed side of a unit's declared reasoning (does,
// model, effort, tools), which the queue shows beside it.
type Reasoning struct {
	// Does is "model": a session's attempt is model work.
	Does string `json:"does"`
	// Model is the model that wrote most of the attempt's output; Models
	// every model it used, when it used more than one.
	Model  string   `json:"model,omitempty"`
	Models []string `json:"models,omitempty"`
	// Effort is the effort level in force at its last turn; Efforts every
	// level it ran at, when it ran at more than one.
	Effort  string   `json:"effort,omitempty"`
	Efforts []string `json:"efforts,omitempty"`
	// Thinking is the extended-thinking mode it requested (adaptive,
	// enabled, disabled).
	Thinking string `json:"thinking,omitempty"`
	// Tools is the tool profile the unit declared (its name); ToolNames the
	// tools the session offered the model, and MCPServers the MCP servers
	// among them.
	Tools      string   `json:"tools,omitempty"`
	ToolNames  []string `json:"toolNames,omitempty"`
	MCPServers []string `json:"mcpServers,omitempty"`
}

// TranscriptUsage totals the token use a Claude Code transcript records
// between byte offsets from and to (to <= 0: its end), per model. A
// transcript writes one API message as several lines, each carrying the
// message's usage, so each message ID counts once. A session's own count is
// tokens, not dollars: it sets no cost.
//
// It also reads what the attempt reasoned with (Claude Code 2.1.303's
// transcript): each assistant line's effort, each API request's thinking
// mode, and the tools of the session's latest prompt snapshot before to,
// which may come before from (a session states its tools once, at its
// start, and again only when they change).
func TranscriptUsage(path string, from, to int64) (*UsageDoc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var r io.Reader = f
	if to > 0 {
		r = io.LimitReader(f, max(to, 0))
	}
	type line struct {
		Type   string `json:"type"`
		Effort string `json:"effort"`
		Params *struct {
			Thinking *struct {
				Type string `json:"type"`
			} `json:"thinking"`
		} `json:"params"`
		Attachment *struct {
			Type  string `json:"type"`
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"attachment"`
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
	var efforts []string
	var effort, thinking string
	var tools []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	var at int64
	for sc.Scan() {
		start := at
		at += int64(len(sc.Bytes())) + 1
		var l line
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		if a := l.Attachment; l.Type == "attachment" && a != nil && a.Type == "prompt_snapshot" && len(a.Tools) > 0 {
			tools = tools[:0]
			for _, t := range a.Tools {
				tools = append(tools, t.Name)
			}
		}
		if start < from {
			continue
		}
		if l.Type == "api-request" && l.Params != nil && l.Params.Thinking != nil && l.Params.Thinking.Type != "" {
			thinking = l.Params.Thinking.Type
		}
		if l.Type == "assistant" && l.Effort != "" {
			effort = l.Effort
			if !slices.Contains(efforts, l.Effort) {
				efforts = append(efforts, l.Effort)
			}
		}
		if l.Type != "assistant" || l.Message.Usage == nil || l.Message.Model == "" {
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
	d.Reasoning = reasoningOf(d.Models, effort, efforts, thinking, tools)
	return d, nil
}

func reasoningOf(models []ModelUsage, effort string, efforts []string, thinking string, tools []string) *Reasoning {
	rs := &Reasoning{Does: "model", Effort: effort, Thinking: thinking}
	var most int64 = -1
	for _, m := range models {
		if m.OutputTokens > most {
			rs.Model, most = m.Model, m.OutputTokens
		}
	}
	if len(models) > 1 {
		for _, m := range models {
			rs.Models = append(rs.Models, m.Model)
		}
	}
	if len(efforts) > 1 {
		rs.Efforts = efforts
	}
	for _, t := range tools {
		rs.ToolNames = append(rs.ToolNames, t)
		// an MCP tool is mcp__<server>__<tool>
		if rest, ok := strings.CutPrefix(t, "mcp__"); ok {
			if server, _, ok := strings.Cut(rest, "__"); ok && !slices.Contains(rs.MCPServers, server) {
				rs.MCPServers = append(rs.MCPServers, server)
			}
		}
	}
	sort.Strings(rs.ToolNames)
	sort.Strings(rs.MCPServers)
	return rs
}

// AttemptStart is where this attempt's turns begin in the member's
// transcript: the offset of the checkpoint before cp in the same session
// (the one the attempt resumed from), or 0 when cp starts its session's
// chain. A block's session starts with its first attempt, so that attempt
// counts from 0, not from where its registration happened to find the
// transcript (the model may already have answered by then).
func (s *Store) AttemptStart(name string, cp *Checkpoint) int64 {
	cps, err := s.readLog(name)
	if err != nil || cp == nil {
		return 0
	}
	var prev *Checkpoint
	for i, c := range cps {
		if c.Session == cp.Session && c.Seq < cp.Seq {
			prev = &cps[i]
		}
	}
	if prev == nil {
		return 0
	}
	if prev.Event == "register" {
		if m, err := s.Get(name); err == nil && m.Block {
			return 0
		}
	}
	return prev.Offset
}
