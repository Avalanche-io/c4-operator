package telemetry

import (
	"encoding/hex"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

// Version is a parsed semantic version.
type Version struct {
	Major, Minor, Patch uint16
	// Dev says the build is not a tagged release.
	Dev bool
}

// Build is the build a record came from, read from runtime/debug only.
type Build struct {
	Version Version
	Go      Version
	// Commit is the VCS revision, when the binary knows it.
	Commit   [20]byte
	Modified bool
	OS       GOOS
	Arch     GOARCH
	AMD64    GOAMD64
}

// CurrentBuild is this binary's build.
func CurrentBuild() Build {
	b := Build{OS: GOOS(enumIndex(goosTable, runtime.GOOS)), Arch: GOARCH(enumIndex(goarchTable, runtime.GOARCH)), Go: parseVersion(runtime.Version())}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		b.Version.Dev = true
		return b
	}
	b.Version = parseVersion(info.Main.Version)
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if raw, err := hex.DecodeString(s.Value); err == nil && len(raw) == len(b.Commit) {
				copy(b.Commit[:], raw)
			}
		case "vcs.modified":
			b.Modified = s.Value == "true"
		case "GOAMD64":
			b.AMD64 = GOAMD64(enumIndex(goamd64Table, s.Value))
		}
	}
	return b
}

// parseVersion reads "v1.2.3", "go1.26.2" and the like; anything else is
// a development build.
func parseVersion(s string) Version {
	s = strings.TrimPrefix(strings.TrimPrefix(s, "go"), "v")
	core, rest, _ := strings.Cut(s, "-")
	parts := strings.Split(core, ".")
	var v Version
	nums := []*uint16{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		if i >= len(nums) {
			break
		}
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil {
			return Version{Dev: true}
		}
		*nums[i] = uint16(n)
	}
	v.Dev = rest != "" || len(parts) < 2
	return v
}
