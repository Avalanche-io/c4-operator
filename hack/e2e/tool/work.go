package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The P5 workloads. Each reads the unit's environment from the queue
// (C4_FRAME_START, C4_SHARD, C4_STEPS...) and spends time the way a real
// unit would, deterministically, so the estimator has something to learn.

// frame renders the frames of one unit: it takes cost seconds per frame,
// scaled by the factor of any heavy range the frame falls in, then writes
// the frame and a small PNG preview into DIR.
//
//	c4e2e frame --out /show/renders/sh040 [--cost 6s] [--heavy 1100-1150:4] [--kib 256]
func frameCmd(args []string) error {
	fs := flag.NewFlagSet("frame", flag.ExitOnError)
	out := fs.String("out", "", "output directory")
	cost := fs.Duration("cost", 6*time.Second, "time per frame outside heavy ranges")
	heavy := fs.String("heavy", "", "comma-separated FROM-TO:FACTOR ranges")
	kib := fs.Int("kib", 256, "frame size in KiB")
	fs.Parse(args)
	start, err1 := strconv.Atoi(os.Getenv("C4_FRAME_START"))
	end, err2 := strconv.Atoi(os.Getenv("C4_FRAME_END"))
	if err1 != nil || err2 != nil || *out == "" {
		return errors.New("usage: c4e2e frame --out DIR (with C4_FRAME_START and C4_FRAME_END set)")
	}
	ranges, err := parseHeavy(*heavy)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(*out, "preview"), 0o777); err != nil {
		return err
	}
	for f := start; f <= end; f++ {
		factor := 1.0
		for _, r := range ranges {
			if f >= r.from && f <= r.to {
				factor = r.factor
			}
		}
		// A deterministic wobble of up to 10% per frame.
		d := time.Duration(float64(*cost) * factor * (0.9 + 0.2*unitRand("frame", f)))
		time.Sleep(d)
		name := fmt.Sprintf("%s.%04d.exr", filepath.Base(*out), f)
		if err := writeKeyed(filepath.Join(*out, name), fmt.Sprintf("%s-%d", *out, f), int64(*kib)*1024); err != nil {
			return err
		}
		var buf bytes.Buffer
		if err := png.Encode(&buf, preview(f, start, factor)); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*out, "preview", fmt.Sprintf("%04d.png", f)), buf.Bytes(), 0o666); err != nil {
			return err
		}
		fmt.Printf("frame %d: %s (factor %.1f)\n", f, d.Round(time.Millisecond), factor)
	}
	return nil
}

type heavyRange struct {
	from, to int
	factor   float64
}

func parseHeavy(s string) ([]heavyRange, error) {
	var out []heavyRange
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		span, f, ok := strings.Cut(item, ":")
		a, b, ok2 := strings.Cut(span, "-")
		from, e1 := strconv.Atoi(a)
		to, e2 := strconv.Atoi(b)
		factor, e3 := strconv.ParseFloat(f, 64)
		if !ok || !ok2 || e1 != nil || e2 != nil || e3 != nil {
			return nil, fmt.Errorf("--heavy %q: want FROM-TO:FACTOR", item)
		}
		out = append(out, heavyRange{from, to, factor})
	}
	return out, nil
}

// unitRand is a deterministic number in [0, 1) for a key and an index.
func unitRand(key string, i int) float64 {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", key, i)))
	return float64(binary.BigEndian.Uint64(h[:8])>>11) / (1 << 53)
}

// preview draws a 320x180 frame: a sky gradient that moves with the frame
// number, warmer where the frame is heavy, and the frame number in
// seven-segment digits.
func preview(frame, first int, factor float64) image.Image {
	const w, h = 320, 180
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	phase := float64(frame-first) / 48
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := float64(y) / h
			s := 0.5 + 0.5*math.Sin(float64(x)/40+phase*2*math.Pi)
			r := uint8(40 + 60*t + 30*s + 40*math.Min(factor-1, 3))
			g := uint8(60 + 80*t + 20*s)
			b := uint8(110 + 100*(1-t))
			img.Set(x, y, color.RGBA{r, g, b, 255})
		}
	}
	digits := strconv.Itoa(frame)
	x0 := w - 16 - len(digits)*22
	for i, ch := range digits {
		drawDigit(img, x0+i*22, h-44, int(ch-'0'))
	}
	return img
}

// segments of each digit: a b c d e f g.
var segs = [10]uint8{0x3f, 0x06, 0x5b, 0x4f, 0x66, 0x6d, 0x7d, 0x07, 0x7f, 0x6f}

func drawDigit(img *image.RGBA, x, y, d int) {
	on := color.RGBA{245, 245, 240, 255}
	bar := func(x0, y0, x1, y1 int) {
		for yy := y0; yy < y1; yy++ {
			for xx := x0; xx < x1; xx++ {
				img.Set(xx, yy, on)
			}
		}
	}
	s := segs[d]
	if s&0x01 != 0 {
		bar(x+2, y, x+16, y+3)
	}
	if s&0x02 != 0 {
		bar(x+15, y+2, x+18, y+15)
	}
	if s&0x04 != 0 {
		bar(x+15, y+17, x+18, y+30)
	}
	if s&0x08 != 0 {
		bar(x+2, y+29, x+16, y+32)
	}
	if s&0x10 != 0 {
		bar(x, y+17, x+3, y+30)
	}
	if s&0x20 != 0 {
		bar(x, y+2, x+3, y+15)
	}
	if s&0x40 != 0 {
		bar(x+2, y+14, x+16, y+17)
	}
}

// shard runs one CI test shard: a duration drawn per shard around the
// mean, and a failure with probability flaky per attempt.
//
//	c4e2e shard [--mean 20s] [--flaky 0.05]
func shardCmd(args []string) error {
	fs := flag.NewFlagSet("shard", flag.ExitOnError)
	mean := fs.Duration("mean", 20*time.Second, "mean shard duration")
	flaky := fs.Float64("flaky", 0, "failure probability per attempt")
	fs.Parse(args)
	shard, err := strconv.Atoi(os.Getenv("C4_SHARD"))
	if err != nil {
		return errors.New("C4_SHARD is not set")
	}
	attempt, _ := strconv.Atoi(os.Getenv("C4_ATTEMPT"))
	key := os.Getenv("C4_JOB")
	d := time.Duration(float64(*mean) * (0.5 + unitRand(key, shard)))
	time.Sleep(d)
	if unitRand(key+"/flaky", shard*100+attempt) < *flaky {
		return fmt.Errorf("shard %d failed on attempt %d after %s", shard, attempt, d.Round(time.Millisecond))
	}
	fmt.Printf("shard %d passed in %s\n", shard, d.Round(time.Millisecond))
	return nil
}

// train runs a training loop that checkpoints into DIR every N steps and
// resumes from the newest checkpoint there. Asked to yield (the queue's
// annotation, seen through the downward API file in C4_PODINFO), it
// finishes the step, saves a checkpoint and exits; capture keeps the
// checkpoint, and the next attempt mounts the captured view.
//
//	c4e2e train --dir /data [--step 200ms] [--kib 512]
func trainCmd(args []string) error {
	fs := flag.NewFlagSet("train", flag.ExitOnError)
	dir := fs.String("dir", "", "checkpoint directory")
	step := fs.Duration("step", 200*time.Millisecond, "time per step")
	kib := fs.Int("kib", 512, "checkpoint size in KiB")
	fs.Parse(args)
	steps, err1 := strconv.Atoi(os.Getenv("C4_STEPS"))
	every, err2 := strconv.Atoi(os.Getenv("C4_CHECKPOINT_EVERY"))
	if err1 != nil || err2 != nil || *dir == "" {
		return errors.New("usage: c4e2e train --dir DIR (with C4_STEPS and C4_CHECKPOINT_EVERY set)")
	}
	ck := filepath.Join(*dir, "ckpt")
	if err := os.MkdirAll(ck, 0o777); err != nil {
		return err
	}
	from := 0
	if b, err := os.ReadFile(filepath.Join(ck, "latest")); err == nil {
		from, _ = strconv.Atoi(strings.TrimSpace(string(b)))
	}
	fmt.Printf("training from step %d of %d\n", from, steps)
	save := func(s int) error {
		name := fmt.Sprintf("step-%07d.bin", s)
		if err := writeKeyed(filepath.Join(ck, name), fmt.Sprintf("%s-%d", os.Getenv("C4_JOB"), s), int64(*kib)*1024); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(ck, "latest"), []byte(strconv.Itoa(s)+"\n"), 0o666)
	}
	podinfo := os.Getenv("C4_PODINFO")
	for s := from + 1; s <= steps; s++ {
		time.Sleep(*step)
		saved := s%every == 0 || s == steps
		if saved {
			if err := save(s); err != nil {
				return err
			}
		}
		if s < steps && podinfo != "" && yieldAsked(podinfo) {
			if !saved {
				if err := save(s); err != nil {
					return err
				}
			}
			fmt.Printf("yielded at step %d\n", s)
			return nil
		}
	}
	fmt.Printf("finished %d steps\n", steps)
	return nil
}

func yieldAsked(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && bytes.Contains(b, []byte("c4.avalanche.io/yield="))
}
