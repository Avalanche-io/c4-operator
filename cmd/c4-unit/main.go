// Command c4-unit is the helper every C4 job image carries, for the parts
// of the unit contract a script would otherwise spell out: progress, frame
// previews, yielding at a checkpoint, and resuming.
//
//	c4-unit progress 0.4            write the unit's progress (0 to 1)
//	c4-unit preview IMAGE FRAME     write preview/FRAME.png for the HUD's player
//	c4-unit yield-requested         exit 0 if the farm asked this unit to yield
//	c4-unit safepoint [--checkpoint REF] [--wait 20s]
//	                                at a safe point, ask the farm whether to stop
//	                                here: exit 10 (and print "stop") or 0 ("continue")
//	c4-unit resume-dir              print the writable mount if this attempt resumes
//	c4-unit output                  print the writable mount
//
// The writable mount is $C4_OUTPUT, else --dir, else the one writable C4
// view found in the container's mounts.
//
// A safe point is a moment the unit could stop and lose nothing it wants
// kept: the end of an agent turn, a frame or chunk boundary, a training
// checkpoint. Arriving at one is a decision point: the unit asks, and the
// farm decides then whether it stops there or goes on.
//
//	c4-unit safepoint --checkpoint step-4000; [ $? -eq 10 ] && exit 0
//
// The request goes through the node daemon to the queue, and the answer
// comes back in $C4_CONTROL/safepoint.decision, within --wait. A soft stop
// the farm asked for earlier (the c4.avalanche.io/yield annotation, with
// its deadline in c4.avalanche.io/yield-by) is advisory: if capacity has
// returned or stopping is no longer worth it, the answer is continue. Only
// when no answer arrives in time does the command fall back on that
// request, seen through $C4_PODINFO: stop if one is still pending. After a
// soft stop's deadline the unit is stopped anyway.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"image"
	_ "image/jpeg"
	"image/png"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	yieldAnnotation   = "c4.avalanche.io/yield"
	yieldByAnnotation = "c4.avalanche.io/yield-by"
	// stopCode is safepoint's exit status when the unit should stop.
	stopCode = 10
)

func main() {
	code, err := run(os.Args[1:], os.Stdout, env{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "c4-unit:", err)
	}
	os.Exit(code)
}

// env is the process environment and filesystem, swappable in tests.
type env struct {
	getenv    func(string) string
	mountinfo string // path of the mount table
	oiiotool  string // the converter for EXR and TIFF
	now       func() time.Time
	sleep     func(time.Duration)
}

func (e env) get(k string) string {
	if e.getenv != nil {
		return e.getenv(k)
	}
	return os.Getenv(k)
}

func run(args []string, stdout io.Writer, e env) (int, error) {
	fs := flag.NewFlagSet("c4-unit", flag.ContinueOnError)
	dir := fs.String("dir", "", "the unit's writable mount (default $C4_OUTPUT, or found from the mounts)")
	width := fs.Int("width", 960, "preview width in pixels")
	checkpoint := fs.String("checkpoint", "", "with safepoint: what the unit saved there (a step, a path, a session turn), for the record")
	wait := fs.Duration("wait", 20*time.Second, "with safepoint: how long to wait for the farm's decision before falling back on its last request")
	if len(args) == 0 {
		return 2, errors.New("usage: c4-unit progress N | preview IMAGE FRAME | yield-requested | safepoint [--checkpoint REF] | resume-dir | output")
	}
	cmd := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return 2, err
	}
	pos := fs.Args()
	switch cmd {
	case "progress":
		if len(pos) != 1 {
			return 2, errors.New("usage: c4-unit progress N (0 to 1)")
		}
		f, err := strconv.ParseFloat(pos[0], 64)
		if err != nil || f < 0 || f > 1 {
			return 2, fmt.Errorf("progress %q: a number from 0 to 1", pos[0])
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		return code(writeAtomic(filepath.Join(out, ".c4", "progress"), []byte(strconv.FormatFloat(f, 'f', -1, 64)+"\n")))
	case "preview":
		if len(pos) != 2 {
			return 2, errors.New("usage: c4-unit preview IMAGE FRAME")
		}
		frame, err := strconv.Atoi(pos[1])
		if err != nil {
			return 2, fmt.Errorf("frame %q is not a number", pos[1])
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		return code(preview(e, pos[0], filepath.Join(out, "preview", strconv.Itoa(frame)+".png"), *width))
	case "safepoint":
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		seq, err := safepoint(e, filepath.Join(out, ".c4", "safepoint"), *checkpoint)
		if err != nil {
			return 1, err
		}
		if d, ok := awaitDecision(e, seq, *wait); ok {
			if d.Do == "stop" {
				fmt.Fprintln(stdout, "stop"+suffix(d.Why))
				return stopCode, nil
			}
			fmt.Fprintln(stdout, "continue"+suffix(d.Why))
			return 0, nil
		}
		// No answer in time: the last request the farm made stands.
		note := " [no decision in " + wait.String() + "]"
		if e.get("C4_CONTROL") == "" {
			note = " [no decision channel]"
		}
		stop, err := softStop(e)
		if err != nil {
			return 1, err
		}
		if stop.asked {
			fmt.Fprintln(stdout, stop.String()+note)
			return stopCode, nil
		}
		fmt.Fprintln(stdout, "continue"+note)
		return 0, nil
	case "yield-requested":
		asked, err := yieldRequested(e)
		if err != nil {
			return 1, err
		}
		if asked {
			return 0, nil
		}
		return 1, nil
	case "resume-dir":
		if e.get("C4_RESUME_VIEW") == "" {
			return 0, nil
		}
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, out)
		return 0, nil
	case "output":
		out, err := output(e, *dir)
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(stdout, out)
		return 0, nil
	}
	return 2, fmt.Errorf("no command %q: progress, preview, yield-requested, safepoint, resume-dir or output", cmd)
}

func code(err error) (int, error) {
	if err != nil {
		return 1, err
	}
	return 0, nil
}

// output finds the unit's writable mount.
func output(e env, dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	if d := e.get("C4_OUTPUT"); d != "" {
		return d, nil
	}
	mi := e.mountinfo
	if mi == "" {
		mi = "/proc/self/mountinfo"
	}
	f, err := os.Open(mi)
	if err != nil {
		return "", errors.New("no writable mount: set C4_OUTPUT or pass --dir")
	}
	defer f.Close()
	var found []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		// mountinfo: id parent major:minor root mountpoint options ... - fstype source superoptions
		fields := strings.Fields(sc.Text())
		sep := -1
		for i, x := range fields {
			if x == "-" {
				sep = i
				break
			}
		}
		if sep < 0 || sep+2 >= len(fields) || len(fields) < 6 {
			continue
		}
		mp, opts, fstype, super := fields[4], fields[5], fields[sep+1], strings.Join(fields[sep+2:], " ")
		// a C4 view is an overlay whose layers live in the node's c4 cache
		if fstype != "overlay" || mp == "/" || !strings.Contains(super, "/var/lib/c4/") || !strings.HasPrefix(opts, "rw") {
			continue
		}
		found = append(found, unescape(mp))
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", errors.New("no writable C4 view is mounted: set C4_OUTPUT or pass --dir")
	}
	return "", fmt.Errorf("more than one writable C4 view (%s): pass --dir", strings.Join(found, ", "))
}

// unescape undoes mountinfo's octal escapes (\040 for a space).
func unescape(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// yieldRequested reports whether the farm asked the unit to stop.
func yieldRequested(e env) (bool, error) {
	s, err := softStop(e)
	return s.asked, err
}

// stopRequest is the farm's soft stop, from the pod's annotations.
type stopRequest struct {
	asked      bool
	reason, by string
}

func (s stopRequest) String() string {
	out := "stop"
	if s.reason != "" {
		out += ": " + s.reason
	}
	if s.by != "" {
		out += " (by " + s.by + ")"
	}
	return out
}

// softStop reads the pod's annotations from the downward API file
// (key="value" lines).
func softStop(e env) (stopRequest, error) {
	p := e.get("C4_PODINFO")
	if p == "" {
		return stopRequest{}, nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return stopRequest{}, err
	}
	var s stopRequest
	for _, l := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		if u, err := strconv.Unquote(strings.TrimSpace(v)); err == nil {
			v = u
		}
		switch strings.TrimSpace(k) {
		case yieldAnnotation:
			s.asked, s.reason = true, v
		case yieldByAnnotation:
			s.by = v
		}
	}
	return s, nil
}

// decision is the farm's answer at a safe point
// ($C4_CONTROL/safepoint.decision, written by the node daemon).
type decision struct {
	Seq int    `json:"seq"`
	Do  string `json:"do"`
	Why string `json:"why"`
}

func suffix(why string) string {
	if why == "" {
		return ""
	}
	return ": " + why
}

// awaitDecision waits up to wait for the farm's answer to safe point seq.
func awaitDecision(e env, seq int, wait time.Duration) (decision, bool) {
	dir := e.get("C4_CONTROL")
	if dir == "" || wait <= 0 {
		return decision{}, false
	}
	now, sleep := time.Now, time.Sleep
	if e.now != nil {
		now = e.now
	}
	if e.sleep != nil {
		sleep = e.sleep
	}
	until := now().Add(wait)
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "safepoint.decision")); err == nil {
			var d decision
			if json.Unmarshal(b, &d) == nil && d.Seq == seq && (d.Do == "stop" || d.Do == "continue") {
				return d, true
			}
		}
		if !now().Before(until) {
			return decision{}, false
		}
		sleep(200 * time.Millisecond)
	}
}

// safepoint records a safe point in path: its sequence number (one past
// the last), the time, and what was saved there, and returns the number.
// The node daemon asks the farm for a decision on it.
func safepoint(e env, path, checkpoint string) (int, error) {
	seq := 1
	if b, err := os.ReadFile(path); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "seq "); ok {
				if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
					seq = n + 1
				}
			}
		}
	}
	at := time.Now()
	if e.now != nil {
		at = e.now()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "seq %d\nat %s\n", seq, at.UTC().Format(time.RFC3339Nano))
	if checkpoint != "" {
		fmt.Fprintf(&b, "checkpoint %s\n", strings.ReplaceAll(checkpoint, "\n", " "))
	}
	return seq, writeAtomic(path, []byte(b.String()))
}

// preview writes a PNG preview of an image. PNG and JPEG are converted
// here; EXR, TIFF and anything else go through oiiotool, color-managed
// from ACES to the sRGB display with OCIO's built-in studio config.
func preview(e env, src, dst string, width int) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	switch strings.ToLower(filepath.Ext(src)) {
	case ".png", ".jpg", ".jpeg":
		return previewNative(src, dst, width)
	}
	tool := e.oiiotool
	if tool == "" {
		tool = "oiiotool"
	}
	tmp := dst + ".tmp.png"
	cmd := exec.Command(tool, src, "--colorconfig", "ocio://studio-config-latest",
		"--ociodisplay", "sRGB - Display", "ACES 1.0 - SDR Video",
		"--resize", fmt.Sprintf("%dx0", width), "-d", "uint8", "-o", tmp)
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%s: %w", tool, err)
	}
	return os.Rename(tmp, dst)
}

// previewNative decodes a PNG or JPEG and writes it, scaled down to width
// by averaging whole-pixel boxes.
func previewNative(src, dst string, width int) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return err
	}
	b := img.Bounds()
	if b.Dx() > width && width > 0 {
		img = scale(img, width)
	}
	tmp := dst + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if err := png.Encode(out, img); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dst)
}

func scale(src image.Image, w int) image.Image {
	b := src.Bounds()
	h := max(1, b.Dy()*w/b.Dx())
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		y0, y1 := b.Min.Y+y*b.Dy()/h, b.Min.Y+max((y+1)*b.Dy()/h, y*b.Dy()/h+1)
		for x := range w {
			x0, x1 := b.Min.X+x*b.Dx()/w, b.Min.X+max((x+1)*b.Dx()/w, x*b.Dx()/w+1)
			var r, g, bl, a, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					cr, cg, cb, ca := src.At(xx, yy).RGBA()
					r, g, bl, a, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), a+uint64(ca), n+1
				}
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = uint8(r/n>>8), uint8(g/n>>8), uint8(bl/n>>8), uint8(a/n>>8)
		}
	}
	return dst
}

// writeAtomic writes a small file so readers never see it half written.
func writeAtomic(p string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}
