// Package frames counts frame ranges like 1001-1048.
package frames

import (
	"fmt"
	"strconv"
	"strings"
)

// Count is the number of frames in a range such as "1001-1048".
func Count(spec string) (int, error) {
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		b = a
	}
	first, err := strconv.Atoi(a)
	if err != nil {
		return 0, fmt.Errorf("%q: %w", spec, err)
	}
	last, err := strconv.Atoi(b)
	if err != nil || last < first {
		return 0, fmt.Errorf("%q is not a range", spec)
	}
	return last - first + 1, nil
}
