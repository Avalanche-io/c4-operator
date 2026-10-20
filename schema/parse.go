package schema

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	yaml "go.yaml.in/yaml/v3"
)

// ParseSubmission reads a submission (YAML or JSON) and checks its shape.
// The errors are the farm's, in its words, sorted by path; with any, the
// submission is nil. What needs the farm (that an after names a job, that a
// tool takes the parameters given) is the farm's check, not this.
func ParseSubmission(b []byte) (*Submission, []Error) {
	var s Submission
	if errs := parse(b, "submit", &s); errs != nil {
		return nil, errs
	}
	return &s, nil
}

// ParseTool reads a tool definition (YAML or JSON) and checks its shape, as
// ParseSubmission does.
func ParseTool(b []byte) (*Tool, []Error) {
	var t Tool
	if errs := parse(b, "tool", &t); errs != nil {
		return nil, errs
	}
	return &t, nil
}

// ParseLock reads a tool.lock and checks its shape.
func ParseLock(b []byte) (*Lock, []Error) {
	var l Lock
	if errs := parse(b, "lock", &l); errs != nil {
		return nil, errs
	}
	return &l, nil
}

// LockSchema is the JSON Schema of a tool.lock, as GET /schema/lock serves
// it.
func LockSchema() []byte { return slices.Clone(lock) }

// ToJSON reads YAML (JSON is YAML) and writes it as JSON, keeping keys in
// the order written. Aliases are resolved, timestamps stay the strings
// written, and a key written twice is an error.
func ToJSON(b []byte) ([]byte, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(b, &n); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if n.Kind == 0 {
		return nil, fmt.Errorf("the document is empty")
	}
	if err := writeJSON(&out, &n); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeJSON(w *bytes.Buffer, n *yaml.Node) error {
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) == 0 {
			return fmt.Errorf("the document is empty")
		}
		return writeJSON(w, n.Content[0])
	case yaml.AliasNode:
		return writeJSON(w, n.Alias)
	case yaml.MappingNode:
		w.WriteByte('{')
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: a key must be a plain name", k.Line)
			}
			if k.Value == "<<" {
				return fmt.Errorf("line %d: merge keys (<<) are not read; write the fields out", k.Line)
			}
			if seen[k.Value] {
				return fmt.Errorf("line %d: %q is written twice", k.Line, k.Value)
			}
			seen[k.Value] = true
			if i > 0 {
				w.WriteByte(',')
			}
			kb, _ := json.Marshal(k.Value)
			w.Write(kb)
			w.WriteByte(':')
			if err := writeJSON(w, n.Content[i+1]); err != nil {
				return err
			}
		}
		w.WriteByte('}')
	case yaml.SequenceNode:
		w.WriteByte('[')
		for i, c := range n.Content {
			if i > 0 {
				w.WriteByte(',')
			}
			if err := writeJSON(w, c); err != nil {
				return err
			}
		}
		w.WriteByte(']')
	case yaml.ScalarNode:
		switch n.ShortTag() {
		case "!!null":
			w.WriteString("null")
		case "!!bool", "!!int", "!!float":
			var v any
			if err := n.Decode(&v); err != nil {
				return fmt.Errorf("line %d: %w", n.Line, err)
			}
			b, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("line %d: %q is not a number JSON can hold", n.Line, n.Value)
			}
			w.Write(b)
		default: // !!str, !!timestamp, !!binary: the text as written
			b, _ := json.Marshal(n.Value)
			w.Write(b)
		}
	default:
		return fmt.Errorf("line %d: unreadable YAML", n.Line)
	}
	return nil
}

//go:embed lock-1.schema.json
var lock []byte

var (
	resolvedOnce sync.Once
	schemas      map[string]*jsonschema.Schema
	resolveErr   error
)

func schemaFor(format string) (*jsonschema.Schema, error) {
	resolvedOnce.Do(func() {
		schemas = map[string]*jsonschema.Schema{}
		for f, b := range map[string][]byte{"submit": submit, "tool": tool, "lock": lock, "chain": chainSchema} {
			s := new(jsonschema.Schema)
			if err := json.Unmarshal(b, s); err != nil {
				resolveErr = fmt.Errorf("schema %s: %w", f, err)
				return
			}
			schemas[f] = s
		}
	})
	return schemas[format], resolveErr
}

func parse(b []byte, format string, into any) []Error {
	j, err := ToJSON(b)
	if err != nil {
		return []Error{{Message: "this is not YAML or JSON: " + err.Error(), Fix: "correct the line it names"}}
	}
	d := json.NewDecoder(bytes.NewReader(j))
	d.UseNumber()
	var doc any
	if err := d.Decode(&doc); err != nil {
		return []Error{{Message: "this is not YAML or JSON: " + err.Error(), Fix: "correct the document"}}
	}
	s, err := schemaFor(format)
	if err != nil {
		return []Error{{Message: err.Error(), Fix: "report this: the farm's schema does not load"}}
	}
	c := &checker{format: format, root: s, doc: doc}
	c.walk(doc, s, nil, nil, rootNoun[format], true)
	if len(c.errs) > 0 {
		sort.SliceStable(c.errs, func(i, k int) bool { return c.errs[i].Path < c.errs[k].Path })
		return dedupe(c.errs)
	}
	if err := json.Unmarshal(j, into); err != nil {
		return []Error{{Message: "this could not be read: " + err.Error(), Fix: "report this: the shape was valid"}}
	}
	return nil
}

func dedupe(errs []Error) []Error {
	var out []Error
	seen := map[Error]bool{}
	for _, e := range errs {
		if !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

var rootNoun = map[string]string{"submit": "a submission", "tool": "a tool", "lock": "a tool.lock", "chain": "a chain"}

// nouns name the $defs in errors: "frame" is not a field of a job.
var nouns = map[string]string{
	"job": "a job", "check": "a check", "approve": "an approve", "wait": "a wait", "answers": "answers",
	"use": "a use", "needs": "needs", "runs": "runs", "param": "a parameter", "userInterface": "userInterface",
	"progressRule": "a progress rule", "fileFilter": "a file filter",
	"by": "by", "for": "for", "delegation": "a delegation", "credential": "a credential", "deployment": "a deployment",
}

// elem is one step of a path: a key, or an index (key "").
type elem struct {
	key   string
	index int
}

func render(p []elem) string {
	var b strings.Builder
	for _, e := range p {
		if e.key == "" {
			fmt.Fprintf(&b, "[%d]", e.index)
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(e.key)
	}
	return b.String()
}

func with(p []elem, e elem) []elem { return append(slices.Clone(p), e) }

type checker struct {
	format string
	root   *jsonschema.Schema
	doc    any
	errs   []Error
}

// at reads the document at a path.
func (c *checker) at(p []elem) any {
	v := c.doc
	for _, e := range p {
		switch x := v.(type) {
		case map[string]any:
			v = x[e.key]
		case []any:
			if e.index >= len(x) {
				return nil
			}
			v = x[e.index]
		default:
			return nil
		}
	}
	return v
}

func (c *checker) ref(r string) (*jsonschema.Schema, string) {
	name := strings.TrimPrefix(r, "#/$defs/")
	return c.root.Defs[name], name
}

func jsonType(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case json.Number:
		if _, err := strconv.ParseInt(string(x), 10, 64); err == nil {
			return "integer"
		}
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return "?"
}

func typeOK(v any, s *jsonschema.Schema) bool {
	t := jsonType(v)
	ok := func(want string) bool { return t == want || (want == "number" && t == "integer") }
	if s.Type != "" {
		return ok(s.Type)
	}
	if len(s.Types) == 0 {
		return true
	}
	for _, w := range s.Types {
		if ok(w) {
			return true
		}
	}
	return false
}

// shape is a path with free keys as * and indexes as []: jobs.*.frames.
func shapeOf(sh []string) string {
	var b strings.Builder
	for _, s := range sh {
		if s == "[]" {
			b.WriteString("[]")
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s)
	}
	return b.String()
}

// walk checks v against s, reporting when report is set, and says whether v
// is valid. path is v's place in the document, shape its pattern, noun what
// an object at v is called.
func (c *checker) walk(v any, s *jsonschema.Schema, path []elem, shape []string, noun string, report bool) bool {
	ok := true
	fail := func(msg, fix string, p []elem) {
		ok = false
		if report {
			c.errs = append(c.errs, Error{Path: render(p), Message: msg, Fix: fix})
		}
	}
	if s.Ref != "" {
		def, name := c.ref(s.Ref)
		n := noun
		if nn, found := nouns[name]; found {
			n = nn
		}
		if !c.walk(v, def, path, shape, n, report) {
			ok = false
		}
	}
	if !typeOK(v, s) {
		want := s.Type
		if want == "" {
			want = strings.Join(s.Types, " or ")
		}
		m, f := c.say("type", shape, path, v, map[string]any{"want": want})
		fail(m, f, path)
		return false
	}
	if s.Const != nil && !equal(v, *s.Const) {
		m, f := c.say("const", shape, path, v, map[string]any{"want": *s.Const})
		fail(m, f, path)
	}
	if s.Enum != nil {
		found := false
		for _, e := range s.Enum {
			if equal(v, e) {
				found = true
			}
		}
		if !found {
			m, f := c.say("enum", shape, path, v, map[string]any{"enum": s.Enum})
			fail(m, f, path)
		}
	}
	if str, isStr := v.(string); isStr {
		if s.MinLength != nil && len([]rune(str)) < *s.MinLength {
			m, f := c.say("minLength", shape, path, v, nil)
			fail(m, f, path)
		}
		if s.MaxLength != nil && len([]rune(str)) > *s.MaxLength {
			m, f := c.say("maxLength", shape, path, v, map[string]any{"max": *s.MaxLength})
			fail(m, f, path)
		}
		if s.Pattern != "" && !compiled(s.Pattern).MatchString(str) {
			m, f := c.say("pattern", shape, path, v, map[string]any{"description": s.Description})
			fail(m, f, path)
		}
	}
	if n, isNum := v.(json.Number); isNum {
		x, _ := n.Float64()
		if (s.Minimum != nil && x < *s.Minimum) || (s.ExclusiveMinimum != nil && x <= *s.ExclusiveMinimum) ||
			(s.Maximum != nil && x > *s.Maximum) || (s.ExclusiveMaximum != nil && x >= *s.ExclusiveMaximum) {
			m, f := c.say("range", shape, path, v, map[string]any{"schema": s})
			fail(m, f, path)
		}
	}
	if obj, isObj := v.(map[string]any); isObj {
		if s.MinProperties != nil && len(obj) < *s.MinProperties {
			m, f := c.say("empty", shape, path, v, nil)
			fail(m, f, path)
		}
		for _, r := range s.Required {
			if _, has := obj[r]; !has {
				p := path
				if len(path) == 0 {
					p = []elem{{key: r}}
				}
				m, f := c.say("required", shape, p, v, map[string]any{"field": r, "noun": noun})
				fail(m, f, p)
			}
		}
		for _, k := range sortedKeys(s.DependentRequired) {
			if _, has := obj[k]; !has {
				continue
			}
			for _, need := range s.DependentRequired[k] {
				if _, has := obj[need]; !has {
					m, f := c.say("dependentRequired", append(slices.Clone(shape), k), with(path, elem{key: k}), v, map[string]any{"field": k, "need": need})
					fail(m, f, with(path, elem{key: k}))
				}
			}
		}
		for _, k := range sortedKeys(obj) {
			kp := with(path, elem{key: k})
			if ps, has := s.Properties[k]; has {
				if !c.walk(obj[k], ps, kp, append(slices.Clone(shape), k), noun, report) {
					ok = false
				}
				continue
			}
			ksh := append(slices.Clone(shape), "*")
			if s.PropertyNames != nil && !c.walk(k, s.PropertyNames, kp, ksh, noun, report) {
				ok = false
			}
			switch {
			case s.AdditionalProperties == nil:
			case isFalse(s.AdditionalProperties):
				if s.Properties == nil {
					break
				}
				m, f := c.say("unknown", append(slices.Clone(shape), k), kp, k, map[string]any{"noun": noun, "known": propertyNames(s)})
				fail(m, f, kp)
			default:
				if !c.walk(obj[k], s.AdditionalProperties, kp, ksh, noun, report) {
					ok = false
				}
			}
		}
	}
	if arr, isArr := v.([]any); isArr {
		if s.MinItems != nil && len(arr) < *s.MinItems {
			m, f := c.say("empty", shape, path, v, nil)
			fail(m, f, path)
		}
		if s.UniqueItems {
			for i := range arr {
				for k := 0; k < i; k++ {
					if equal(arr[i], arr[k]) {
						m, f := c.say("unique", shape, with(path, elem{index: i}), arr[i], nil)
						fail(m, f, with(path, elem{index: i}))
					}
				}
			}
		}
		if s.Items != nil {
			for i, it := range arr {
				if !c.walk(it, s.Items, with(path, elem{index: i}), append(slices.Clone(shape), "[]"), noun, report) {
					ok = false
				}
			}
		}
	}
	for _, sub := range s.AllOf {
		if !c.walk(v, sub, path, shape, noun, report) {
			ok = false
		}
	}
	if len(s.AnyOf) > 0 {
		matched := false
		for _, sub := range s.AnyOf {
			if c.walk(v, sub, path, shape, noun, false) {
				matched = true
				break
			}
		}
		if !matched {
			m, f := c.say("anyOf", shape, path, v, map[string]any{"branches": s.AnyOf, "noun": noun})
			fail(m, f, path)
		}
	}
	if len(s.OneOf) > 0 {
		var good []int
		for i, sub := range s.OneOf {
			if c.walk(v, sub, path, shape, noun, false) {
				good = append(good, i)
			}
		}
		switch {
		case len(good) == 1:
		case len(good) > 1:
			m, f := c.say("oneOfMany", shape, path, v, map[string]any{"branches": s.OneOf})
			fail(m, f, path)
		default:
			// Report the branch the value was meant for: the one whose type it has.
			meant := -1
			for i, sub := range s.OneOf {
				if (sub.Type != "" || len(sub.Types) > 0 || sub.Ref != "") && typeOK(v, c.deref(sub)) {
					meant = i
					break
				}
			}
			if meant >= 0 {
				c.walk(v, s.OneOf[meant], path, shape, noun, report)
				ok = false
			} else {
				m, f := c.say("oneOf", shape, path, v, map[string]any{"branches": s.OneOf})
				fail(m, f, path)
			}
		}
	}
	if s.If != nil {
		branch := s.Else
		if c.walk(v, s.If, path, shape, noun, false) {
			branch = s.Then
		}
		if branch != nil && !c.walk(v, branch, path, shape, noun, report) {
			ok = false
		}
	}
	if s.Not != nil && c.walk(v, s.Not, path, shape, noun, false) {
		// not {required: [x]} or not {anyOf: [{required: [x]}, ...]}: x does not belong here.
		forbidden := s.Not.Required
		for _, sub := range s.Not.AnyOf {
			forbidden = append(forbidden, sub.Required...)
		}
		obj, _ := v.(map[string]any)
		reported := false
		for _, f := range forbidden {
			if _, has := obj[f]; has {
				fp := with(path, elem{key: f})
				m, fx := c.say("forbidden", append(slices.Clone(shape), f), fp, obj[f], map[string]any{"field": f})
				fail(m, fx, fp)
				reported = true
			}
		}
		if !reported {
			m, f := c.say("not", shape, path, v, nil)
			fail(m, f, path)
		}
	}
	return ok
}

func (c *checker) deref(s *jsonschema.Schema) *jsonschema.Schema {
	if s.Ref != "" {
		d, _ := c.ref(s.Ref)
		return d
	}
	return s
}

// isFalse says whether s is the schema false, which the library reads as
// {not: {}}.
func isFalse(s *jsonschema.Schema) bool {
	b, _ := json.Marshal(s)
	return string(b) == "false" || string(b) == `{"not":{}}`
}

func propertyNames(s *jsonschema.Schema) []string {
	if len(s.PropertyOrder) > 0 {
		return s.PropertyOrder
	}
	return sortedKeys(s.Properties)
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func equal(a, b any) bool {
	ab, _ := json.Marshal(norm(a))
	bb, _ := json.Marshal(norm(b))
	return bytes.Equal(ab, bb)
}

func norm(v any) any {
	if n, ok := v.(json.Number); ok {
		f, _ := n.Float64()
		return f
	}
	return v
}

func quote(v any) string {
	switch x := v.(type) {
	case string:
		return strconv.Quote(x)
	case json.Number:
		return x.String()
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// closest is the candidate nearest to s (at most two edits away, or a
// prefix), or "".
func closest(s string, cands []string) string {
	best, bestD := "", 3
	for _, c := range cands {
		d := edits(strings.ToLower(s), strings.ToLower(c))
		if d < bestD || (d == bestD && best == "") {
			best, bestD = c, d
		}
	}
	if best == "" {
		for _, c := range cands {
			if strings.HasPrefix(c, s) || strings.HasPrefix(s, c) {
				return c
			}
		}
	}
	return best
}

func edits(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func last(p []elem) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i].key != "" {
			return p[i].key
		}
	}
	return "it"
}

func article(t string) string {
	if strings.HasPrefix(t, "INT") {
		return "an " + t
	}
	return "a " + t
}

var patterns sync.Map

func compiled(p string) *regexp.Regexp {
	if r, ok := patterns.Load(p); ok {
		return r.(*regexp.Regexp)
	}
	r := regexp.MustCompile(p)
	patterns.Store(p, r)
	return r
}

var quantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// say is every message the farm gives for a document's shape.
func (c *checker) say(kind string, shape []string, path []elem, v any, ctx map[string]any) (string, string) {
	sh := shapeOf(shape)
	field := last(path)
	switch kind + " " + c.format + " " + sh {
	// A submission.
	case "unknown submit jobs.*.priority", "unknown submit priority":
		return "a job has no priority: work is ordered by when it is due",
			"say when it is needed with due: a milestone, a time, a date or suggested"
	case "unknown submit jobs.*.image", "unknown submit jobs.*.command":
		return "a submission names tools, never images",
			"publish a tool that runs the image (c4 farm tool new), and name it in tool:"
	case "unknown submit jobs.*.needs.licences", "unknown submit jobs.*.needs.network":
		return field + " are the tool's to declare, not a submission's",
			"ask the tool's owner to add them to its needs, or choose one of its sizes"
	case "anyOf submit jobs.*":
		return "a job needs a tool, or a plan with each", "add tool: <name>; c4 farm tools lists them"
	case "pattern submit jobs.*.answers.by":
		if s, _ := v.(string); strings.HasPrefix(s, "person:") || strings.HasPrefix(s, "role:") {
			return "a person is never asked mid-run: questions go to an agent",
				"name an agent (lead, tool:<name> or unit:<name>); for a person's decision, add an approve step"
		}
	case "pattern submit for":
		if s, _ := v.(string); strings.Contains(s, ":") {
			kind, name, _ := strings.Cut(s, ":")
			if kind != "person" && kind != "org" && kind != "organization" {
				return fmt.Sprintf("%s: for names a party, a person or an organization, never a %s", quote(v), kind), "name the person or organization the work is for"
			}
			return fmt.Sprintf("%s: for names a party by its name alone", quote(v)), "write for: " + name
		}
	case "pattern submit root":
		return fmt.Sprintf("%s is not an absolute path", quote(v)), "write the folder the tree came from, as S:/shows/saltmarsh or /Users/noor/shows/saltmarsh"
	case "dependentRequired submit jobs.*.combine":
		return "combine needs over: the values it combines", "add over:, or remove combine"
	case "dependentRequired submit jobs.*.each":
		return "each needs a plan: the blocks it runs on", "add plan:, or use tool: for a job without one"
	case "oneOfMany submit jobs.*.wait":
		return "a wait is on someone (for) or a time (until), not both", "keep one of them"
	case "oneOf submit jobs.*.wait":
		return "a wait needs for (a party or a person) or until (a time)", "add for: party:<name>, or until: <a time>"
	// A tool.
	case "enum tool kind":
		if v == "render" {
			return `"render" is not a kind`, "a render is kind: frames"
		}
		return quote(v) + " is not a kind", "the kinds are frames, once, shards, generator, check, agent and train"
	case "anyOf tool runs":
		return "runs needs an image, a host, or both", "add image: <an image>, or host: {has: [...]}"
	case "type tool runs.command":
		return "a command is a list of arguments, never one shell string", `write [bash, "{{Tool.Dir}}/run.sh"]`
	case "forbidden tool emits":
		return "only a generator emits", "set kind: generator, or remove emits"
	case "forbidden tool judges", "forbidden tool rework":
		return "only a check " + map[string]string{"judges": "judges its units", "rework": "reworks"}[field],
			"set kind: check, or remove " + field
	case "forbidden tool frames":
		return "only a frames or generator tool has frame defaults", "remove frames, or change the kind"
	case "enum tool params[].type":
		s, _ := v.(string)
		if up := strings.ToUpper(s); up != s && slices.Contains([]string{"STRING", "PATH", "INT", "FLOAT", "BOOL", "RANGE_EXPR"}, up) {
			return quote(v) + " is not a parameter type", "types are OpenJD's, in capitals: " + up
		}
		return quote(v) + " is not a parameter type", "the types are STRING, PATH, INT, FLOAT, BOOL, RANGE_EXPR and LIST[...]"
	case "pattern tool params[].name":
		s, _ := v.(string)
		alt := regexp.MustCompile(`[^A-Za-z0-9_]`).ReplaceAllString(s, "_")
		return quote(v) + " is not a parameter name", "use letters, digits and _, as " + alt
	case "forbidden tool params[].objectType", "forbidden tool params[].dataFlow":
		return field + " is for PATH parameters", "remove it, or make the parameter a PATH"
	case "enum tool params[].userInterface.control":
		ptype, _ := c.at(path[:len(path)-2]).(map[string]any)["type"].(string)
		var draw []string
		for _, e := range ctx["enum"].([]any) {
			if e != "HIDDEN" {
				draw = append(draw, e.(string))
			}
		}
		return fmt.Sprintf("%v is not a control for %s", v, article(ptype)),
			fmt.Sprintf("%s is drawn with %s", article(ptype), strings.Join(draw, " or "))
	case "not tool runs.env.*", "not tool runs.sidecars[].env.*":
		return "C4_ variables are the farm's own", fmt.Sprintf("use another name; the farm sets %v itself", v)
	case "pattern tool needs.licences.*":
		return "a licence is named pool/feature", fmt.Sprintf("for example sesi/%v: 1", v)
	case "oneOf tool progress[]":
		return "a progress rule says which group is the percent or the frame", "add percent: 1 or frame: 1"
	case "oneOfMany tool progress[]":
		return "a progress rule reads a percent or a frame, not both", "keep one of them"
	case "pattern tool previews":
		return fmt.Sprintf("%s has no frame number", quote(v)), "write where the frame number goes, as render/*.$F4.exr"
	case "required chain for":
		if ctx["field"] == "party" {
			return "an accountable chain is for a party", "name the party in for.party, or set accountable: false with its gap"
		}
	case "required chain on[]":
		return "an accountable chain's deployments each name their binding and owner", "add binding and owner (the farm binding covers every deployment), or set accountable: false"
	case "forbidden chain using[].holderSubject":
		return "an accountable chain's credentials are each held by a party", "name the holder's party, or set accountable: false"
	case "required chain by":
		if ctx["field"] == "person" {
			return "signed is for a party's own key: a deployment's or an agent's own record is direct", "use grade: direct"
		}
	case "pattern chain by.deployment", "pattern chain on[].deployment":
		return fmt.Sprintf("%s is not a deployment", quote(v)), "write <farm>/<kind>/<name>, kind one of component, node, runner, sync or import"
	case "oneOf chain using[]":
		return "a credential names who holds it", "add holder: (a party), or holderSubject: (who holds it, when not a party yet)"
	case "pattern chain for.party":
		return fmt.Sprintf("%s is not a party: a party is a person or an organization", quote(v)), "name the person or organization; an agent or unit goes in by"
	case "anyOf chain by":
		return "by names who acted: a person, a unit, an agent, a deployment or a tracker user", "add one of person, unit, agent, deployment or user"
	case "pattern chain by.unit":
		return fmt.Sprintf("%s is not a unit: it needs its job's UID", quote(v)), "write <project>/<job>@<job UID>/<index>, as show-a/github-7@3f9a1c/3"
	case "forbidden chain gap":
		return "an accountable chain has no gap", "remove gap, or set accountable: false"
	case "enum tool needs.farm":
		return quote(v) + " is not a farm access", "a tool may have read, job or project; administration is a person's"
	case "required tool runs.sidecars[]":
		if ctx["field"] == "port" {
			return "a sidecar needs the port it listens on", "add port: <the port>; the unit finds it at http://localhost:<port>"
		}
	}
	// Anywhere.
	switch {
	case kind == "required" && c.format == "chain" && ctx["field"] == "gap":
		return "an unaccountable chain says why", "add gap: the first link that failed, in words"
	case kind == "required" && len(path) == 1 && ctx["field"] == "c4":
		return "the file has no c4 field, so it is not " + rootNoun[c.format],
			fmt.Sprintf("add c4: %s as the first line", map[string]string{"submit": "submit/1", "tool": "tool/1", "lock": "lock/1"}[c.format])
	case kind == "const" && sh == "c4":
		return fmt.Sprintf("%v is not a format this farm reads", v), fmt.Sprintf("this farm reads %v", ctx["want"])
	case kind == "pattern" && (field == "frames" || strings.HasSuffix(sh, ".frames") || field == "preview"):
		s, _ := v.(string)
		fix := "write frames as 1001-1240, 1001-1240:2 or 1001,1050-1100"
		if strings.Contains(s, "..") {
			fix = "write a range with a dash: " + strings.ReplaceAll(s, "..", "-")
		}
		if field == "preview" {
			return quote(v) + " is not a preview", "use fml, every:<n>, a frame list, or none"
		}
		return quote(v) + " is not a frame list", fix
	case kind == "type" && quantity.MatchString(fmt.Sprint(v)) && (field == "memory" || field == "gpuMemory" || field == "scratch"):
		return field + " needs a unit", fmt.Sprintf("write %vG", v)
	}
	switch kind {
	case "unknown":
		known := ctx["known"].([]string)
		fix := "the fields of " + ctx["noun"].(string) + " are " + strings.Join(known, ", ")
		if m := closest(fmt.Sprint(v), known); m != "" {
			fix = fmt.Sprintf("did you mean %q?", m)
		}
		return fmt.Sprintf("%q is not a field of %s", v, ctx["noun"]), fix
	case "required":
		return fmt.Sprintf("%s needs %s", ctx["noun"], ctx["field"]), fmt.Sprintf("add %s:", ctx["field"])
	case "type":
		return fmt.Sprintf("%s is a %s here, not a %s", field, ctx["want"], jsonType(v)), "see GET /schema/" + c.format
	case "enum":
		var opts []string
		for _, e := range ctx["enum"].([]any) {
			opts = append(opts, fmt.Sprint(e))
		}
		fix := "use one of them"
		if m := closest(fmt.Sprint(v), opts); m != "" {
			fix = fmt.Sprintf("did you mean %q?", m)
		}
		return fmt.Sprintf("%s is not one of %s", quote(v), strings.Join(opts, ", ")), fix
	case "const":
		return fmt.Sprintf("%s must be %v", field, ctx["want"]), fmt.Sprintf("write %s: %v", field, ctx["want"])
	case "pattern":
		fix := "see GET /schema/" + c.format
		if d, _ := ctx["description"].(string); d != "" {
			fix = d
		}
		if len(shape) > 0 && shape[len(shape)-1] == "*" {
			return fmt.Sprintf("%s is not a name %s takes", quote(v), last(path[:len(path)-1])), fix
		}
		return fmt.Sprintf("%s is not a valid %s", quote(v), field), fix
	case "minLength", "empty":
		return field + " is empty", "give it a value, or leave it out"
	case "maxLength":
		return fmt.Sprintf("%s is longer than %v characters", field, ctx["max"]), "shorten it"
	case "range":
		return fmt.Sprintf("%v is out of range for %s", v, field), "see GET /schema/" + c.format
	case "unique":
		return fmt.Sprintf("%s is listed twice", quote(v)), "list it once"
	case "dependentRequired":
		return fmt.Sprintf("%s needs %s", ctx["field"], ctx["need"]), fmt.Sprintf("add %s:, or remove %s", ctx["need"], ctx["field"])
	case "forbidden":
		return fmt.Sprintf("%s does not belong here", field), "remove it"
	case "anyOf", "oneOf":
		return fmt.Sprintf("%s is not any of the forms it can take", field), "see GET /schema/" + c.format
	case "oneOfMany":
		return fmt.Sprintf("%s matches more than one form", field), "keep one of them"
	}
	return fmt.Sprintf("%s is not valid", field), "see GET /schema/" + c.format
}
