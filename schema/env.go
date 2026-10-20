package schema

import (
	"strings"
	"unicode"
)

// ParamEnv is the variable a tool's unit gets a parameter as: C4_PARAM_
// and the name in upper snake case (frameRange and frame_range are both
// C4_PARAM_FRAME_RANGE). The farm refuses a tool whose names collide so,
// and the tool runner sets them; both use this.
func ParamEnv(name string) string {
	var b strings.Builder
	b.WriteString("C4_PARAM_")
	rs := []rune(name)
	for i, r := range rs {
		if unicode.IsUpper(r) && i > 0 && (unicode.IsLower(rs[i-1]) || unicode.IsDigit(rs[i-1])) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToUpper(r))
	}
	return b.String()
}
