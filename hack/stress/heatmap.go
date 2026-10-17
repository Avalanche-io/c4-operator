package main

import (
	"fmt"
	"html"
	"math"
	"strings"
)

// Heatmaps are SVG: a grid of cells over two binned axes, each cell's
// mean value written in it and its range in a hover title. Colors are a
// one-hue sequential ramp (light to dark with magnitude) or, for a signed
// difference, a blue-red diverging ramp with a gray midpoint.

type rampKind int

const (
	rampBlue rampKind = iota
	rampOrange
	rampDiverging
)

var (
	blueRamp   = []string{"#cde2fb", "#9ec5f4", "#6da7ec", "#3987e5", "#256abf", "#184f95", "#0d366b"}
	orangeRamp = []string{"#fde4d8", "#f9bfa3", "#f39367", "#eb6834", "#c4501f", "#943a14", "#64260b"}
	redRamp    = []string{"#fbe0e0", "#f4aeae", "#ec7c7b", "#e34948", "#b83232", "#8a2222", "#5e1515"}
	neutral    = "#f0efec"
	surface    = "#fcfcfb"
	inkPrimary = "#0b0b0b"
	inkSecond  = "#52514e"
)

type cellAcc struct {
	sum float64
	n   int
}

type heatGrid struct {
	nx, ny int
	c      [][]cellAcc
}

func newGrid(nx, ny int) *heatGrid {
	g := &heatGrid{nx: nx, ny: ny, c: make([][]cellAcc, nx)}
	for i := range g.c {
		g.c[i] = make([]cellAcc, ny)
	}
	return g
}

func (g *heatGrid) add(x, y int, v float64) {
	g.c[x][y].sum += v
	g.c[x][y].n++
}

func hexRGB(h string) (float64, float64, float64) {
	var r, g, b int
	fmt.Sscanf(h, "#%02x%02x%02x", &r, &g, &b)
	return float64(r), float64(g), float64(b)
}

// rampAt interpolates a ramp at t in [0,1].
func rampAt(ramp []string, t float64) string {
	t = math.Max(0, math.Min(1, t))
	pos := t * float64(len(ramp)-1)
	i := int(math.Floor(pos))
	if i >= len(ramp)-1 {
		return ramp[len(ramp)-1]
	}
	f := pos - float64(i)
	r0, g0, b0 := hexRGB(ramp[i])
	r1, g1, b1 := hexRGB(ramp[i+1])
	return fmt.Sprintf("#%02x%02x%02x", int(r0+(r1-r0)*f), int(g0+(g1-g0)*f), int(b0+(b1-b0)*f))
}

func luminance(h string) float64 {
	r, g, b := hexRGB(h)
	lin := func(c float64) float64 {
		c /= 255
		if c <= 0.03928 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func edgeLabel(x float64) string {
	switch {
	case x >= 1000:
		return fmt.Sprintf("%.0fk", x/1000)
	case x == math.Trunc(x):
		return fmt.Sprintf("%.0f", x)
	}
	return fmt.Sprintf("%.2g", x)
}

func heatmapSVG(title, xName, yName string, xe, ye []float64, g *heatGrid, kind rampKind, format string) string {
	const cw, ch, left, top, gap = 76, 46, 150, 56, 2
	w := left + g.nx*cw + 24
	h := top + g.ny*ch + 70
	lo, hi := math.Inf(1), math.Inf(-1)
	for x := range g.nx {
		for y := range g.ny {
			if c := g.c[x][y]; c.n > 0 {
				v := c.sum / float64(c.n)
				lo, hi = math.Min(lo, v), math.Max(hi, v)
			}
		}
	}
	scale := func(v float64) string {
		switch kind {
		case rampDiverging:
			m := math.Max(math.Abs(lo), math.Abs(hi))
			if m == 0 || math.IsInf(m, 0) {
				return neutral
			}
			t := v / m
			if math.Abs(t) < 0.05 {
				return neutral
			}
			if t > 0 {
				return rampAt(blueRamp, t)
			}
			return rampAt(redRamp, -t)
		case rampOrange:
			if hi <= 0 {
				return rampAt(orangeRamp, 0)
			}
			return rampAt(orangeRamp, v/hi)
		}
		if hi == lo {
			return rampAt(blueRamp, 0.5)
		}
		return rampAt(blueRamp, (v-lo)/(hi-lo))
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" font-family="-apple-system, 'Segoe UI', Helvetica, Arial, sans-serif">`, w, h, w, h)
	fmt.Fprintf(&b, `<rect width="100%%" height="100%%" fill="%s"/>`, surface)
	fmt.Fprintf(&b, `<text x="16" y="26" font-size="15" font-weight="600" fill="%s">%s</text>`, inkPrimary, html.EscapeString(title))
	fmt.Fprintf(&b, `<text x="16" y="44" font-size="11" fill="%s">rows: %s; columns: %s</text>`, inkSecond, html.EscapeString(yName), html.EscapeString(xName))
	for y := range g.ny {
		// Rows run top to bottom from the highest bin.
		row := g.ny - 1 - y
		cy := top + row*ch
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" text-anchor="end" fill="%s">%s–%s</text>`, left-8, cy+ch/2+4, inkSecond, edgeLabel(ye[y]), edgeLabel(ye[y+1]))
		for x := range g.nx {
			cx := left + x*cw
			c := g.c[x][y]
			rng := fmt.Sprintf("%s %s–%s, %s %s–%s", xName, edgeLabel(xe[x]), edgeLabel(xe[x+1]), yName, edgeLabel(ye[y]), edgeLabel(ye[y+1]))
			if c.n == 0 {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%d" height="%d" rx="4" fill="none" stroke="#e4e3df" stroke-dasharray="3 3"><title>%s: no points</title></rect>`,
					cx+gap/2, cy+gap/2, cw-gap, ch-gap, html.EscapeString(rng))
				continue
			}
			v := c.sum / float64(c.n)
			fill := scale(v)
			ink := inkPrimary
			if luminance(fill) < 0.22 {
				ink = "#ffffff"
			}
			val := fmt.Sprintf(format, v)
			fmt.Fprintf(&b, `<g><title>%s: %s (%d points)</title><rect x="%d" y="%d" width="%d" height="%d" rx="4" fill="%s"/>`,
				html.EscapeString(rng), val, c.n, cx+gap/2, cy+gap/2, cw-gap, ch-gap, fill)
			fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="13" font-weight="600" text-anchor="middle" fill="%s">%s</text>`, cx+cw/2, cy+ch/2+1, ink, val)
			fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="9" text-anchor="middle" fill="%s">n=%d</text></g>`, cx+cw/2, cy+ch/2+14, ink, c.n)
		}
	}
	by := top + g.ny*ch
	for x := 0; x <= g.nx; x++ {
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" text-anchor="middle" fill="%s">%s</text>`, left+x*cw, by+16, inkSecond, edgeLabel(xe[x]))
	}
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" text-anchor="middle" fill="%s">%s</text>`, left+g.nx*cw/2, by+36, inkSecond, html.EscapeString(xName))
	fmt.Fprintf(&b, `<text x="16" y="%d" font-size="10" fill="%s">%s</text>`, h-12, inkSecond, legendNote(kind, lo, hi, format))
	b.WriteString(`</svg>`)
	return b.String()
}

func legendNote(kind rampKind, lo, hi float64, format string) string {
	if math.IsInf(lo, 0) {
		return "no data"
	}
	switch kind {
	case rampDiverging:
		return "blue: the policy ahead of the best baseline; red: behind; gray: within 5% of the largest difference"
	case rampOrange:
		return fmt.Sprintf("darker: more failing invariants (most %s)", fmt.Sprintf(format, hi))
	}
	return fmt.Sprintf("darker is higher; range %s to %s", fmt.Sprintf(format, lo), fmt.Sprintf(format, hi))
}
