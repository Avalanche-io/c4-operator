package authoring

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxJobName is the longest a WorkJob's name may be: it labels and
// prefixes its unit pods.
const MaxJobName = 50

var versionSuffix = regexp.MustCompile(`-v(\d+)$`)

// NextName is <stem>-<stage>-vNNN, one version past the latest job of that
// stem and stage among names (the jobs of a namespace), as POST /jobs
// names a job it is given no name for.
func NextName(names []string, stem, stage string) string {
	base := strings.Trim(strings.ToLower(stem+"-"+stage), "-")
	if base == "" {
		base = "job"
	}
	top := 0
	for _, n := range names {
		if !strings.HasPrefix(n, base+"-v") {
			continue
		}
		if m := versionSuffix.FindStringSubmatch(n); m != nil && n == base+m[0] {
			if v, _ := strconv.Atoi(m[1]); v > top {
				top = v
			}
		}
	}
	return fmt.Sprintf("%s-v%03d", base, top+1)
}

// Latest is the job name names: the name itself when it is a job, else
// the latest version of it (<name>-vNNN), else "".
func Latest(names []string, name string) string {
	best, top := "", -1
	for _, n := range names {
		if n == name {
			return n
		}
		if m := versionSuffix.FindStringSubmatch(n); m != nil && n == name+m[0] {
			if v, _ := strconv.Atoi(m[1]); v > top {
				best, top = n, v
			}
		}
	}
	return best
}

// capName is a name cut to MaxJobName, ending in a short hash of the whole
// so two long names stay apart (as c4-sync's jobDNS does).
func capName(s string) string {
	if len(s) <= MaxJobName {
		return s
	}
	h := sha256.Sum256([]byte(s))
	return strings.TrimRight(s[:39], "-") + "-" + hex.EncodeToString(h[:])[:10]
}
