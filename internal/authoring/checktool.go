package authoring

import (
	"errors"
	"fmt"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"

	"github.com/Avalanche-io/c4-operator/schema"
)

// formatNames are the format strings a tool may use besides
// {{Param.<name>}} (the tool format: schema/README.md).
var formatNames = []string{"Unit.Frames", "Unit.FrameStart", "Unit.FrameEnd", "Unit.FrameCount", "Unit.Name", "Unit.Index",
	"Job.Tree", "Job.Shot", "Job.Name", "Tool.Dir", "Run.Exe", "Run.PathMap"}

var formatRe = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// CheckTool is the refusals of a parsed tool that need the farm or a
// look across fields: two parameters of one name, a default outside its
// allowed values, a format string or parameter that does not exist, a
// pattern that is not a regular expression, and emits naming a tool the
// project does not have.
func CheckTool(t *Tool, project string, farm Farm) []Error {
	var errs []Error
	params := map[string]bool{}
	envs := map[string]string{}
	var names []string
	for i, p := range t.Params {
		if params[p.Name] {
			errs = append(errs, errf(fmt.Sprintf("params[%d].name", i), "rename or remove one of them", "a second parameter named %q", p.Name))
			continue
		}
		params[p.Name] = true
		env := ParamEnv(p.Name)
		if other, ok := envs[env]; ok {
			errs = append(errs, errf(fmt.Sprintf("params[%d].name", i), "rename one of them", "%s and %s are both %s", p.Name, other, env))
			continue
		}
		envs[env] = p.Name
		names = append(names, p.Name)
		if p.Default != nil && len(p.AllowedValues) > 0 && !allowed(p.Default, p.AllowedValues) {
			errs = append(errs, errf(fmt.Sprintf("params[%d].default", i), "add it to allowedValues, or choose one of them",
				"the default %s is not one of its allowedValues", show(p.Default)))
		}
	}
	format := func(path, s string) {
		for _, m := range formatRe.FindAllStringSubmatch(s, -1) {
			name := m[1]
			if p, ok := strings.CutPrefix(name, "Param."); ok {
				if !params[p] {
					fix := "its parameters are " + strings.Join(sortedStrings(names), ", ")
					if len(names) == 0 {
						fix = "add the parameter to params"
					} else if c, ok := closest(p, names); ok {
						fix = fmt.Sprintf("did you mean %q?", c)
					}
					errs = append(errs, errf(path, fix, "no parameter %q", p))
				}
				continue
			}
			if !has(formatNames, name) {
				fix := "format strings are Param.<name>, " + strings.Join(formatNames, ", ")
				if c, ok := closestFormat(name); ok {
					fix = fmt.Sprintf("did you mean %q?", c)
				}
				errs = append(errs, errf(path, fix, "%q is not a format string", name))
			}
		}
	}
	for i, a := range t.Runs.Command {
		format(fmt.Sprintf("runs.command[%d]", i), a)
	}
	for _, k := range sortedKeys(t.Runs.Env) {
		format("runs.env."+k, t.Runs.Env[k])
	}
	if t.Runs.Workdir != "" {
		format("runs.workdir", t.Runs.Workdir)
	}
	for _, k := range sortedKeys(t.AIUsage) {
		if s, ok := t.AIUsage[k].(string); ok {
			format("aiUsage."+k, s)
		}
	}
	for i, r := range t.Progress {
		if e, bad := regexError(fmt.Sprintf("progress[%d].match", i), r.Match); bad {
			errs = append(errs, e)
		}
	}
	if es := t.Errors; true {
		for _, g := range []struct {
			name string
			pats []string
		}{{"licence", es.Licence}, {"permanent", es.Permanent}, {"question", es.Question}, {"retry", es.Retry}, {"ignore", es.Ignore}} {
			for i, p := range g.pats {
				if e, bad := regexError(fmt.Sprintf("errors.%s[%d]", g.name, i), p); bad {
					errs = append(errs, e)
				}
			}
		}
	}
	if len(t.Emits) > 0 && farm != nil {
		known := toolNames(farm.Tools(project))
		for i, e := range t.Emits {
			if e == t.Name || has(known, e) {
				continue
			}
			fix := "publish it first; c4 farm tools lists them"
			if c, ok := closest(e, known); ok {
				fix = fmt.Sprintf("did you mean %q?", c)
			}
			errs = append(errs, errf(fmt.Sprintf("emits[%d]", i), fix, "no tool %q in %s", e, project))
		}
	}
	return errs
}

// closestFormat prefers a format string whose last part contains the
// name's (Unit.Start for Unit.FrameStart), then the nearest.
func closestFormat(name string) (string, bool) {
	scope, last, _ := strings.Cut(name, ".")
	var hits []string
	for _, f := range formatNames {
		fs, fl, _ := strings.Cut(f, ".")
		if strings.EqualFold(fs, scope) && last != "" && strings.Contains(strings.ToLower(fl), strings.ToLower(last)) {
			hits = append(hits, f)
		}
	}
	if len(hits) > 0 {
		sort.Strings(hits)
		return hits[0], true
	}
	return closest(name, formatNames)
}

func regexError(path, pat string) (Error, bool) {
	_, err := regexp.Compile(pat)
	if err == nil {
		return Error{}, false
	}
	var se *syntax.Error
	code := err.Error()
	fix := "patterns are RE2 regular expressions"
	if errors.As(err, &se) {
		code = se.Code.String()
		if se.Code == syntax.ErrMissingParen {
			fix = "close the group: " + closeGroup(pat)
		}
	}
	return errf(path, fix, "this is not a regular expression: %s", code), true
}

// closeGroup closes the last open group where its pattern ends: after the
// escapes, classes and quantifiers that follow its (.
func closeGroup(p string) string {
	i := strings.LastIndex(p, "(")
	if i < 0 {
		return p + ")"
	}
	j := i + 1
	for j < len(p) {
		switch c := p[j]; {
		case c == '\\' && j+1 < len(p):
			j += 2
		case c == '[':
			k := strings.IndexByte(p[j:], ']')
			if k < 0 {
				return p + ")"
			}
			j += k + 1
		case strings.IndexByte("+*?.", c) >= 0:
			j++
		default:
			return p[:j] + ")" + p[j:]
		}
	}
	return p + ")"
}

func allowed(v any, vals []any) bool {
	for _, a := range vals {
		if fmt.Sprint(plain(a)) == fmt.Sprint(plain(v)) {
			return true
		}
	}
	return false
}

func show(v any) string {
	if s, ok := v.(string); ok {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprint(plain(v))
}

// ParamEnv is the variable a unit gets a parameter as (schema.ParamEnv).
func ParamEnv(name string) string { return schema.ParamEnv(name) }
