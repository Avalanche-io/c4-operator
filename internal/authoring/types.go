package authoring

import (
	"encoding/json"

	"github.com/Avalanche-io/c4-operator/schema"
)

// The formats' types are the schema package's, so the server, the runner
// and the CLI read the files alike.
type (
	Submission = schema.Submission
	Job        = schema.Job
	Tool       = schema.Tool
	Param      = schema.Param
	Resources  = schema.Resources
	Lock       = schema.Lock
	Frames     = schema.Frames
)

// ParseSubmission reads a submission, YAML or JSON, strictly, with the
// structural refusals in the farm's words (schema.ParseSubmission). Check
// adds those that need the farm.
func ParseSubmission(b []byte) (*Submission, []Error) { return schema.ParseSubmission(b) }

// ParseTool reads a tool.yaml (schema.ParseTool). CheckTool adds the
// refusals that need the farm or a look across fields.
func ParseTool(b []byte) (*Tool, []Error) { return schema.ParseTool(b) }

// ParseLock reads a tool.lock (schema.ParseLock).
func ParseLock(b []byte) (*Lock, []Error) { return schema.ParseLock(b) }

// plain is v with JSON numbers as int64 or float64 and nested values
// alike, for comparing and writing values a format leaves open.
func plain(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := map[string]any{}
		for k, e := range x {
			m[k] = plain(e)
		}
		return m
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = plain(e)
		}
		return out
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case float64:
		if x == float64(int64(x)) {
			return int64(x)
		}
	}
	return v
}
