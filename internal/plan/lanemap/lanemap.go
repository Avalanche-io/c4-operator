package lanemap

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Avalanche-io/c4-operator/internal/plan"
)

// Import reads a plan written as a lane map: a plan directory holding
// LANES.md, a milestone file such as M1.md, and charters/.
// milestone selects the charters and the milestone file ("M1").
//
// What it reads, each edge citing its sentence:
//   - LANES.md's lane tables: every lane, its chapter, rule count, status,
//     charter, and the families it owns; the M2 column's "after" lists;
//   - the ordering sentences of "The lanes", "The five merges" and "Run
//     order": "X follows Y", "X cannot start before Y merges", "X precedes
//     Y", "X lands before Y starts", "X starts after Y merges", "X starts
//     when Y … landed", families that "come after" other families;
//   - the run order block, as each lane's runOrder;
//   - the fillers ("… as fillers");
//   - the milestone file's "Order and parallelism" section: waves, "after"
//     in parentheses, "then" and "once … lands";
//   - each charter: model, refutation and the paths it is granted (holds).
//
// Anything with ordering words that it could not read is a Note.
func Import(dir, milestone string) (*plan.Plan, error) {
	p := plan.New()
	lanesPath := filepath.Join(dir, "LANES.md")
	ls, err := readLines(lanesPath)
	if err != nil {
		return nil, err
	}
	r := &reader{p: p, dir: dir, fams: map[string]string{}}
	lanesFile := rel(dir, lanesPath)

	// two passes: every lane first, then the M2 column, whose ranges
	// ("after C4.1 … C4.6") need the lanes of later rows
	for _, pass := range []bool{false, true} {
		r.laneTable(lanesFile, section(ls, "The lanes"), pass)
		r.laneTable(lanesFile, section(ls, "Lanes that exist because"), pass)
	}
	r.prose(lanesFile, section(ls, "The lanes"), false)
	r.prose(lanesFile, section(ls, "The five merges"), true)
	r.prose(lanesFile, section(ls, "Run order"), true)
	r.runOrder(section(ls, "Run order"))

	mPath := filepath.Join(dir, milestone+".md")
	if ms, err := readLines(mPath); err == nil {
		r.waves(rel(dir, mPath), section(ms, "8. Order and parallelism"))
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := r.charters(filepath.Join(dir, "charters"), milestone); err != nil {
		return nil, err
	}
	r.resolve()
	// every other lane came from a later need: a register row, a lane's
	// follow-on, a lane chartered because another needed it
	for _, n := range p.Names() {
		if u := p.Unit(n); u.Params["origin"] == "" {
			u.Params["origin"] = "follow-on"
		}
	}
	return p, nil
}

type reader struct {
	p    *plan.Plan
	dir  string
	fams map[string]string // rule family → owning lane
	// pending edges, resolved once every lane is known
	edges []pendingEdge
	// prev is the first lane the previous sentence named
	prev string
	// chapterRefs records each bare chapter reference and what it was read as
	chapterRefs []string
}

type pendingEdge struct {
	block, dep string
	cite       plan.Cite
}

var (
	boldLane  = regexp.MustCompile(`\*\*(?:M1-)?(M2-\d+|[ABCFZ]\d+(?:\.\d+[a-z]?|[a-z])?)\*\*`)
	slugRe    = regexp.MustCompile("^\\s*`([a-z0-9-]+)`")
	mergedRe  = regexp.MustCompile("merged (?:`)?([0-9a-f]{7,40})(?:`)?")
	charterRe = regexp.MustCompile(`docs/plan/charters/([A-Za-z0-9.-]+\.md)`)
	familyRe  = regexp.MustCompile(`\b([A-Z]{2,5}) \d+\b`)
	chapterRe = regexp.MustCompile(`^(\d+)\b`)
)

// laneTable reads the rows of a lane table.
func (r *reader) laneTable(file string, ls []line, m2 bool) {
	chapter := ""
	for _, l := range ls {
		t := strings.TrimSpace(l.text)
		if !strings.HasPrefix(t, "|") || strings.HasPrefix(t, "| -") || strings.HasPrefix(t, "|--") {
			continue
		}
		c := cells(t)
		if len(c) < 2 {
			continue
		}
		if m := chapterRe.FindStringSubmatch(c[0]); m != nil {
			chapter = "chapter " + m[1]
		}
		// the lane is the first bold lane name in the row's first two cells
		cell := c[0]
		m := boldLane.FindStringSubmatchIndex(cell)
		if m == nil && len(c) > 1 {
			cell = c[1]
			m = boldLane.FindStringSubmatchIndex(cell)
		}
		if m == nil {
			continue
		}
		name := cell[m[2]:m[3]]
		// the plan writes a lane's deferral in capitals; a lower-case
		// "deferred to M2" is about a part of the lane
		if strings.Contains(t, "DEFERRED TO M2") && !strings.HasPrefix(name, "M2-") {
			if !m2 {
				r.p.Note(file, l.n, clip(t), "deferred to M2: left out of this milestone")
			}
			continue
		}
		if m2 {
			if len(c) >= 6 && cell == c[1] {
				r.m2Cell(file, l.n, chapter, clean(c[5]))
			}
			continue
		}
		group := chapter
		if !strings.HasPrefix(name, "C") && !strings.HasPrefix(name, "M2-") {
			group = groupOf(name)
		}
		if strings.HasPrefix(name, "M2-") {
			group = "chapter " + strings.TrimPrefix(name, "M2-")
		}
		u := r.lane(name, group)
		rest := cell[m[1]:]
		if s := slugRe.FindStringSubmatch(rest); s != nil {
			u.Label = name + " " + s[1]
			u.Params["slug"] = s[1]
		}
		if mm := mergedRe.FindStringSubmatch(t); mm != nil {
			u.Params["status"], u.Params["sha"] = "merged", mm[1]
		} else if strings.Contains(t, "merged ") {
			u.Params["status"] = "merged"
		} else if strings.Contains(t, "chartered") {
			u.Params["status"] = "chartered"
		}
		if cm := charterRe.FindStringSubmatch(t); cm != nil {
			u.Params["charter"] = "charters/" + cm[1]
		}
		// a six-column lane row: chapter, lane, section, families, rules, M2
		if len(c) >= 6 && cell == c[1] {
			fms := familyRe.FindAllStringSubmatch(clean(c[3]), -1)
			for _, fm := range fms {
				if _, ok := r.fams[fm[1]]; !ok {
					r.fams[fm[1]] = name
				}
			}
			// a lane that owns whole families was in the plan from the
			// start; one that names single rules came from a later need
			if len(fms) > 0 {
				u.Params["origin"] = "plan"
			}
			if n, ok := leadingInt(clean(c[4])); ok {
				u.Params["rules"] = strconv.Itoa(n)
			}
		}
	}
}

// m2Cell reads "M2-1: after C1.2, C1.3b, C1.4".
func (r *reader) m2Cell(file string, n int, chapter, cell string) {
	name, list, ok := strings.Cut(cell, ":")
	name = strings.TrimSpace(name)
	if !ok || !strings.HasPrefix(name, "M2-") {
		return
	}
	u := r.lane(name, chapter)
	u.Params["target"] = "metal"
	u.Params["origin"] = "plan"
	list = strings.TrimSpace(list)
	after, ok := strings.CutPrefix(list, "after")
	if !ok {
		return
	}
	for _, d := range r.lanesIn(after) {
		r.edges = append(r.edges, pendingEdge{name, d, plan.Cite{File: file, Line: n, Text: cell}})
	}
}

// ordering patterns, on one clause of a cleaned sentence. In each, the
// lanes on one side run after the lanes on the other.
var patterns = []struct {
	re *regexp.Regexp
	// blockSide is the submatch whose lanes run after depSide's
	blockSide, depSide int
}{
	{regexp.MustCompile(`^(.*?)\bcannot start before\b(.*?)\bmerges?\b`), 1, 2},
	{regexp.MustCompile(`^(.*?)\bfollows?\b(.*)$`), 1, 2},
	{regexp.MustCompile(`^(.*?)\bprecedes?\b(.*)$`), 2, 1},
	{regexp.MustCompile(`^(.*?)\blands before\b(.*?)\bstarts\b`), 2, 1},
	{regexp.MustCompile(`^(.*?)\bstarts after\b(.*?)\bmerges?\b`), 1, 2},
	{regexp.MustCompile(`^(.*?)\bstarts when\b(.*?)\b(?:have|has) landed\b`), 1, 2},
	{regexp.MustCompile(`^(.*?)\bcomes? after\b(.*)$`), 1, 2},
}

var orderingWords = regexp.MustCompile(`\b(after|before|follows?|precedes?|then|once|until|waits? on)\b`)

// prose reads the ordering sentences of a section. With notes, a sentence
// that names lanes and uses ordering words but matches no pattern becomes
// a Note.
func (r *reader) prose(file string, ls []line, notes bool) {
	prev := ""
	for _, s := range sentences(ls) {
		r.prev = prev
		matched := false
		for _, cl := range clauses(s.text) {
			if r.clause(file, s.n, s.text, cl) {
				matched = true
			}
		}
		if !matched {
			// a subject split off by a clause boundary: try the whole sentence
			matched = r.clause(file, s.n, s.text, s.text)
		}
		if ts := tokens(s.text); len(ts) > 0 && !isChapterRef(ts[0].name) {
			prev = ts[0].name
		}
		if strings.Contains(s.text, "as fillers") {
			before, _, _ := strings.Cut(s.text, "as fillers")
			for _, t := range tokens(before) {
				if u := r.p.Unit(t.name); u != nil {
					u.Params["filler"] = "true"
				}
			}
			matched = true
		}
		if notes && !matched && orderingWords.MatchString(s.text) && len(r.lanesIn(s.text))+len(r.familyLanes(s.text)) >= 2 {
			r.p.Note(file, s.n, clip(s.text), "ordering words and lane names, but no pattern the importer reads")
		}
	}
}

func isPronoun(s string) bool {
	w := strings.ToLower(strings.TrimSpace(s))
	return w == "it" || w == "this lane" || w == "the lane"
}

// clauses splits a sentence where a new subject may start.
func clauses(s string) []string {
	var out []string
	for _, part := range strings.Split(s, "; ") {
		out = append(out, strings.Split(part, ", and ")...)
	}
	return out
}

// clause applies the ordering patterns to one clause.
func (r *reader) clause(file string, n int, sent, cl string) bool {
	for _, pt := range patterns {
		m := pt.re.FindStringSubmatch(cl)
		if m == nil {
			continue
		}
		r.chapterRefs = r.chapterRefs[:0]
		blocks := r.sideLanes(m[pt.blockSide])
		deps := r.sideLanes(m[pt.depSide])
		// "It starts after B8 merges": the lane the previous sentence named
		if pt.blockSide == 1 && len(blocks) == 0 && isPronoun(m[1]) && r.prev != "" {
			blocks = []string{r.prev}
		}
		if len(blocks) == 0 || len(deps) == 0 {
			continue
		}
		for _, b := range blocks {
			for _, d := range deps {
				r.edges = append(r.edges, pendingEdge{b, d, plan.Cite{File: file, Line: n, Text: clip(sent)}})
			}
		}
		for _, cr := range r.chapterRefs {
			r.p.Note(file, n, clip(sent), cr+"; check that the plan meant all of them")
		}
		return true
	}
	return false
}

// sideLanes are the lanes one side of a pattern names: lane names, or, when
// it names none, the lanes owning the rule families it names.
func (r *reader) sideLanes(s string) []string {
	if ls := r.lanesIn(s); len(ls) > 0 {
		return ls
	}
	return r.familyLanes(s)
}

func (r *reader) familyLanes(s string) []string {
	var out []string
	for _, w := range regexp.MustCompile(`\b[A-Z]{2,5}\b`).FindAllString(s, -1) {
		if l, ok := r.fams[w]; ok && !contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// lanesIn lists the lanes s names, expanding a chapter reference (C3) to
// its lanes and a range (C4.1 … C4.6, C4.1–C4.6, C4.1 to C4.6) to the
// lanes between.
func (r *reader) lanesIn(s string) []string {
	ts := tokens(s)
	var out []string
	add := func(n string) {
		if !contains(out, n) {
			out = append(out, n)
		}
	}
	for i, t := range ts {
		if isChapterRef(t.name) {
			ch, _ := strconv.Atoi(t.name[1:])
			var got []string
			for _, n := range r.p.Names() {
				if chapterOf(n) == ch && strings.HasPrefix(n, "C") {
					add(n)
					got = append(got, n)
				}
			}
			r.chapterRefs = append(r.chapterRefs, t.name+" read as chapter "+t.name[1:]+"'s lanes: "+strings.Join(got, ", "))
			continue
		}
		add(t.name)
		if i+1 < len(ts) {
			gap := strings.TrimSpace(s[t.end:ts[i+1].start])
			if gap == "…" || gap == "..." || gap == "–" || gap == "to" {
				for _, n := range r.between(t.name, ts[i+1].name) {
					add(n)
				}
			}
		}
	}
	return out
}

// between lists the known chapter lanes strictly between a and b.
func (r *reader) between(a, b string) []string {
	ca, cb := chapterOf(a), chapterOf(b)
	na, okA := laneNum(a)
	nb, okB := laneNum(b)
	if ca == 0 || ca != cb || !okA || !okB {
		return nil
	}
	var out []string
	for _, n := range r.p.Names() {
		if chapterOf(n) != ca {
			continue
		}
		if k, ok := laneNum(n); ok && k > na && k < nb {
			out = append(out, n)
		}
	}
	return out
}

// runOrder numbers the lanes in the run order block, first seen first.
func (r *reader) runOrder(ls []line) {
	code := false
	seen := 0
	for _, l := range ls {
		t := strings.TrimSpace(l.text)
		if strings.HasPrefix(t, "```") {
			if code {
				return
			}
			code = true
			continue
		}
		if !code {
			continue
		}
		for _, tk := range tokens(clean(t)) {
			u := r.p.Unit(tk.name)
			if u == nil || u.Params["runOrder"] != "" {
				continue
			}
			seen++
			u.Params["runOrder"] = strconv.Itoa(seen)
		}
	}
}

var (
	parenAfter = regexp.MustCompile(`\b((?:M1-)?(?:[ABCFZ]\d+(?:\.\d+[a-z]?|[a-z])?))\s*\(([^)]*)\)`)
	waveRe     = regexp.MustCompile(`wave (\d+)`)
)

// waves reads the milestone file's "Order and parallelism" list: the lanes
// of each wave, "after" in parentheses, "X, then Y", and "Z … once Y lands".
func (r *reader) waves(file string, ls []line) {
	for _, l := range ls {
		t := clean(strings.TrimSpace(l.text))
		if t == "" {
			continue
		}
		wave := ""
		if m := waveRe.FindStringSubmatch(t); m != nil {
			wave = m[1]
		}
		group := ""
		switch {
		case strings.Contains(t, "Phase A") || wave != "":
			group = "phase A"
		case strings.Contains(t, "Phase B"):
			group = "phase B"
		}
		if group == "" {
			continue
		}
		// lanes named in the line, outside its parentheses
		outside := regexp.MustCompile(`\([^)]*\)`).ReplaceAllString(t, "")
		for _, tk := range tokens(outside) {
			if isChapterRef(tk.name) {
				continue
			}
			u := r.lane(tk.name, group)
			u.Params["origin"] = "plan"
			if wave != "" {
				u.Params["wave"] = wave
			}
		}
		for _, m := range parenAfter.FindAllStringSubmatch(t, -1) {
			block := strings.TrimPrefix(m[1], "M1-")
			inner := m[2]
			i := strings.Index(inner, "after ")
			if i < 0 {
				continue
			}
			for _, d := range r.lanesIn(inner[i:]) {
				r.edges = append(r.edges, pendingEdge{block, d, plan.Cite{File: file, Line: l.n, Text: clip(t)}})
			}
		}
		// "B1, then B2, with B3 in parallel with B2 once B1 lands"
		for _, cl := range strings.Split(t, ", with ") {
			if a, b, ok := strings.Cut(cl, ", then "); ok {
				as, bs := r.lanesIn(lastClause(a)), r.lanesIn(b)
				if len(as) > 0 && len(bs) > 0 {
					r.edges = append(r.edges, pendingEdge{bs[0], as[len(as)-1], plan.Cite{File: file, Line: l.n, Text: clip(t)}})
				}
			}
			if a, b, ok := strings.Cut(cl, " once "); ok {
				as, bs := r.lanesIn(a), r.lanesIn(b)
				if len(as) > 0 && len(bs) > 0 {
					for _, d := range bs {
						r.edges = append(r.edges, pendingEdge{as[0], d, plan.Cite{File: file, Line: l.n, Text: clip(t)}})
					}
				}
			}
		}
	}
}

func lastClause(s string) string {
	if i := strings.LastIndex(s, ":"); i >= 0 {
		return s[i+1:]
	}
	return s
}

var (
	charterFile = regexp.MustCompile(`^(M\d)-(M2-\d+|[ABCFZ]\d+(?:\.\d+[a-z]?|[a-z])?)-([a-z0-9.-]+)\.md$`)
	m2File      = regexp.MustCompile(`^(M2-\d+)-([a-z0-9-]+)\.md$`)
	codePath    = regexp.MustCompile("`([^`\\s]+)`")
)

// charters reads each of the milestone's charters: the lane it names, its
// model, its refutation and the paths it is granted.
func (r *reader) charters(dir, milestone string) error {
	ents, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, f := range names {
		var lane string
		if m := charterFile.FindStringSubmatch(f); m != nil && m[1] == milestone {
			lane = m[2]
		} else if m := m2File.FindStringSubmatch(f); m != nil {
			lane = m[1]
		} else {
			continue
		}
		u := r.lane(lane, groupOf(lane))
		u.Params["charter"] = "charters/" + f
		ls, err := readLines(filepath.Join(dir, f))
		if err != nil {
			return err
		}
		inGrants := false
		for _, l := range ls {
			t := strings.TrimSpace(l.text)
			switch {
			case strings.HasPrefix(t, "- **Model:**"):
				u.Params["model"] = clip(clean(strings.TrimSpace(strings.TrimPrefix(t, "- **Model:**"))))
			case strings.HasPrefix(t, "- **Refutation:**"):
				v := strings.ToLower(clean(strings.TrimPrefix(t, "- **Refutation:**")))
				switch {
				case strings.HasPrefix(strings.TrimSpace(v), "full"):
					u.Params["refute"] = "full"
				case strings.Contains(v, "no refuter") || strings.HasPrefix(strings.TrimSpace(v), "none"):
					u.Params["refute"] = "none"
				default:
					u.Params["refute"] = clip(strings.TrimSpace(v))
				}
			}
			if strings.HasPrefix(t, "- **Grants, by file:**") {
				inGrants = true
				continue
			}
			if inGrants {
				if strings.HasPrefix(l.text, "  - ") || strings.HasPrefix(l.text, "    - ") {
					if m := codePath.FindStringSubmatch(t); m != nil {
						u.Holds = append(u.Holds, strings.TrimSuffix(m[1], ":"))
					}
					continue
				}
				inGrants = false
			}
		}
	}
	return nil
}

// resolve turns the pending edges into edges, noting any that name a lane
// the plan does not have.
func (r *reader) resolve() {
	for _, e := range r.edges {
		switch {
		case r.p.Unit(e.block) == nil:
			r.p.Note(e.cite.File, e.cite.Line, e.cite.Text, fmt.Sprintf("names %s, which the plan does not list", e.block))
		case r.p.Unit(e.dep) == nil:
			r.p.Note(e.cite.File, e.cite.Line, e.cite.Text, fmt.Sprintf("names %s, which the plan does not list", e.dep))
		default:
			r.p.Edge(e.block, e.dep, e.cite)
		}
	}
}

// groupOf is a lane's row when no chapter is given.
func groupOf(name string) string {
	switch {
	case strings.HasPrefix(name, "M2-"):
		return "chapter " + strings.TrimPrefix(name, "M2-")
	case strings.HasPrefix(name, "C"):
		return "chapter " + strconv.Itoa(chapterOf(name))
	case strings.HasPrefix(name, "A"), strings.HasPrefix(name, "Z"):
		return "phase A"
	case strings.HasPrefix(name, "B"):
		return "phase B"
	case strings.HasPrefix(name, "F"):
		return "foundations"
	}
	return ""
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// clip keeps a cited sentence readable.
func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 280 {
		return string(r[:279]) + "…"
	}
	return s
}

// lane returns the block for a lane, keyed by its kind (keyOf) when new.
func (r *reader) lane(name, group string) *plan.Unit {
	u := r.p.Block(name, group)
	if u.Key == plan.DefaultKey {
		u.Key = keyOf(name)
	}
	return u
}

// keyOf is the estimator key for a lane in this layout: lanes of one kind
// (infrastructure, front end, foundation, chapter, the M2 targets) share
// history.
func keyOf(name string) string {
	switch {
	case strings.HasPrefix(name, "M2-"):
		return "metal-lane"
	case strings.HasPrefix(name, "C"):
		return "lane"
	case strings.HasPrefix(name, "A"):
		return "infra-lane"
	case strings.HasPrefix(name, "B"):
		return "frontend-lane"
	case strings.HasPrefix(name, "F"):
		return "foundation-lane"
	}
	return "lane"
}
