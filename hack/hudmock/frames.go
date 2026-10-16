package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"strings"
)

// Preview frames are procedural PNGs: the frame number burned in, something
// that moves, and visibly heavier content inside a heavy region.

const pw, ph = 384, 216

type canvas struct{ img *image.RGBA }

func newCanvas() *canvas { return &canvas{img: image.NewRGBA(image.Rect(0, 0, pw, ph))} }

func (c *canvas) set(x, y int, r, g, b float64) {
	if x < 0 || y < 0 || x >= pw || y >= ph {
		return
	}
	c.img.SetRGBA(x, y, color.RGBA{clamp8(r), clamp8(g), clamp8(b), 255})
}

func (c *canvas) add(x, y int, r, g, b float64) {
	if x < 0 || y < 0 || x >= pw || y >= ph {
		return
	}
	o := c.img.RGBAAt(x, y)
	c.set(x, y, float64(o.R)+r, float64(o.G)+g, float64(o.B)+b)
}

func (c *canvas) mix(x, y int, r, g, b, a float64) {
	if x < 0 || y < 0 || x >= pw || y >= ph {
		return
	}
	o := c.img.RGBAAt(x, y)
	c.set(x, y, float64(o.R)*(1-a)+r*a, float64(o.G)*(1-a)+g*a, float64(o.B)*(1-a)+b*a)
}

func clamp8(v float64) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func (c *canvas) rect(x0, y0, x1, y1 int, r, g, b float64) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			c.set(x, y, r, g, b)
		}
	}
}

// 3x5 bitmap glyphs.
var glyphs = map[rune]string{
	'0': "111101101101111", '1': "010110010010111", '2': "111001111100111", '3': "111001111001111",
	'4': "101101111001001", '5': "111100111001111", '6': "111100111101111", '7': "111001010010010",
	'8': "111101111101111", '9': "111101111001111", 'A': "010101111101101", 'B': "110101110101110",
	'C': "011100100100011", 'D': "110101101101110", 'E': "111100110100111", 'F': "111100110100100",
	'G': "011100101101011", 'H': "101101111101101", 'I': "111010010010111", 'J': "001001001101010",
	'K': "101101110101101", 'L': "100100100100111", 'M': "101111111101101", 'N': "110101101101101",
	'O': "010101101101010", 'P': "110101110100100", 'Q': "010101101110011", 'R': "110101110101101",
	'S': "011100010001110", 'T': "111010010010010", 'U': "101101101101111", 'V': "101101101101010",
	'W': "101101111111101", 'X': "101101010101101", 'Y': "101101010010010", 'Z': "111001010100111",
	'-': "000000111000000", '.': "000000000000010", ':': "000010000010000", '/': "001001010100100",
}

func (c *canvas) text(s string, x, y, scale int, r, g, b float64) {
	for _, ch := range strings.ToUpper(s) {
		gl, ok := glyphs[ch]
		if ok {
			for i, bit := range gl {
				if bit != '1' {
					continue
				}
				gx, gy := i%3, i/3
				for dy := 0; dy < scale; dy++ {
					for dx := 0; dx < scale; dx++ {
						c.set(x+gx*scale+dx, y+gy*scale+dy, r, g, b)
					}
				}
			}
		}
		x += 4 * scale
	}
}

func textWidth(s string, scale int) int { return len(s)*4*scale - scale }

// renderFrame draws one preview frame of a job. heavy is 0 outside a heavy
// region and up to 1 inside it.
func renderFrame(job string, frame, first, last int, heavy float64) []byte {
	c := newCanvas()
	t := float64(frame - first)
	switch {
	case strings.HasPrefix(job, "sh050"), strings.HasPrefix(job, "sh010"):
		duskHarbor(c, t, float64(last-first))
	case strings.HasPrefix(job, "ep104"):
		crowdPlaza(c, t)
	default:
		nightTanker(c, t, heavy, frame)
	}
	slate := strings.ToUpper(strings.ReplaceAll(job, "-", " "))
	c.rect(0, ph-22, pw, ph, 0, 0, 0)
	c.text(slate, 8, ph-16, 2, 170, 170, 170)
	fs := fmt.Sprint(frame)
	c.text(fs, pw-8-textWidth(fs, 3), ph-19, 3, 235, 235, 235)
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	_ = enc.Encode(&buf, c.img)
	return buf.Bytes()
}

func nightTanker(c *canvas, t, heavy float64, frame int) {
	horizon := 128
	for y := 0; y < ph; y++ {
		for x := 0; x < pw; x++ {
			if y < horizon {
				f := float64(y) / float64(horizon)
				c.set(x, y, 10+28*f, 14+32*f, 22+38*f)
				continue
			}
			f := float64(y-horizon) / float64(ph-horizon)
			c.set(x, y, 16-6*f, 24-8*f, 32-8*f)
		}
	}
	// Swell: moving highlight lines on the water.
	for k := 0; k < 9; k++ {
		yb := float64(horizon + 4 + k*9 + k*k/2)
		amp := 1.2 + float64(k)*0.5
		for x := 0; x < pw; x++ {
			y := yb + amp*math.Sin(float64(x)/(9+float64(k)*2)+t*0.35+float64(k)*1.7)
			if math.Sin(float64(x)/5+t*0.5+float64(k)) > 0.2 {
				c.add(x, int(y), 30, 38, 44)
			}
		}
	}
	// Tanker drifting slowly left.
	tx := 250 - int(t*0.25)
	c.rect(tx-110, horizon-14, tx+40, horizon+6, 6, 8, 10)
	c.rect(tx+8, horizon-34, tx+32, horizon-14, 7, 9, 11)
	c.rect(tx+18, horizon-46, tx+22, horizon-34, 7, 9, 11)
	for i := 0; i < 5; i++ {
		c.set(tx+12+i*4, horizon-28, 160, 140, 90)
	}
	// Lighthouse and its beam sweeping across the frame.
	lx, ly := 46, horizon-58
	c.rect(lx-5, ly, lx+5, horizon+2, 24, 26, 28)
	c.rect(lx-7, ly-6, lx+7, ly, 30, 32, 34)
	ang := math.Mod(t*0.11, 2*math.Pi)
	dirx, diry := math.Cos(ang), math.Sin(ang)*0.25
	for y := 0; y < horizon+20; y++ {
		for x := 0; x < pw; x++ {
			dx, dy := float64(x-lx), float64(y-ly)
			d := math.Hypot(dx, dy)
			if d < 1 {
				continue
			}
			cos := (dx*dirx + dy*diry) / (d * math.Hypot(dirx, diry))
			if cos < 0.985 {
				continue
			}
			a := (cos - 0.985) / 0.015 * math.Max(0, 1-d/420) * 0.55
			c.add(x, y, 120*a, 118*a, 96*a)
		}
	}
	c.rect(lx-3, ly-4, lx+3, ly, 250, 240, 200)
	if heavy <= 0 {
		return
	}
	// The breach: fire, spray and smoke, denser deeper into the region.
	rng := rand.New(rand.NewSource(int64(frame)))
	bx, by := tx-40, horizon-6
	for i := 0; i < int(2600*heavy); i++ {
		age := rng.Float64()
		x := float64(bx) + rng.NormFloat64()*10*(1+age*3) + age*30
		y := float64(by) - age*90*rng.Float64() - math.Abs(rng.NormFloat64())*6
		a := (1 - age) * 0.6
		switch {
		case age < 0.35:
			c.add(int(x), int(y), 230*a, 120*a, 30*a)
		case age < 0.6:
			c.add(int(x), int(y), 150*a, 60*a, 10*a)
		default:
			c.mix(int(x), int(y), 40, 40, 42, 0.5*heavy)
		}
	}
	for i := 0; i < int(900*heavy); i++ {
		x := float64(bx) + rng.NormFloat64()*40
		y := float64(horizon) + math.Abs(rng.NormFloat64())*12
		c.add(int(x), int(y), 70*heavy, 50*heavy, 30*heavy)
	}
	for i := 0; i < int(500*heavy); i++ {
		x := rng.Float64() * pw
		y := rng.Float64() * ph
		for k := 0; k < 5; k++ {
			c.add(int(x)+k/2, int(y)+k, 18, 20, 24)
		}
	}
}

func duskHarbor(c *canvas, t, span float64) {
	horizon := 120
	for y := 0; y < ph; y++ {
		for x := 0; x < pw; x++ {
			if y < horizon {
				f := float64(y) / float64(horizon)
				c.set(x, y, 60+150*f, 70+80*f, 110-30*f)
				continue
			}
			f := float64(y-horizon) / float64(ph-horizon)
			c.set(x, y, 70-40*f, 60-30*f, 70-20*f)
		}
	}
	sx, sy := 300, horizon-6
	for y := sy - 18; y < sy+18; y++ {
		for x := sx - 18; x < sx+18; x++ {
			if math.Hypot(float64(x-sx), float64(y-sy)) < 16 && y < horizon {
				c.set(x, y, 250, 200, 140)
			}
		}
	}
	for y := horizon; y < ph; y++ {
		w := 10 + float64(y-horizon)*0.6
		for x := sx - int(w); x < sx+int(w); x++ {
			if math.Sin(float64(y)*0.9+t*0.6+float64(x)*0.05) > 0.3 {
				c.add(x, y, 90, 60, 30)
			}
		}
	}
	// Pier and a boat crossing the frame over the shot.
	c.rect(0, horizon-8, 120, horizon-4, 30, 22, 24)
	for x := 6; x < 120; x += 14 {
		c.rect(x, horizon-4, x+3, horizon+18, 30, 22, 24)
	}
	bx := int(-60 + (t/math.Max(span, 1))*(pw+120))
	c.rect(bx, horizon+6, bx+70, horizon+16, 24, 18, 22)
	c.rect(bx+20, horizon-10, bx+44, horizon+6, 28, 22, 26)
	c.rect(bx+31, horizon-34, bx+33, horizon-10, 28, 22, 26)
}

// crowdPlaza is a plaza seen from above, with people crossing it.
func crowdPlaza(c *canvas, t float64) {
	for y := 0; y < ph; y++ {
		for x := 0; x < pw; x++ {
			v := 92 + 14*math.Sin(float64(x)/23)*math.Sin(float64(y)/17)
			if (x/48+y/48)%2 == 0 {
				v += 8
			}
			c.set(x, y, v, v-4, v-10)
		}
	}
	rng := rand.New(rand.NewSource(104))
	for i := 0; i < 420; i++ {
		x0, y0 := rng.Float64()*pw, rng.Float64()*(ph-24)
		ang := rng.Float64() * 2 * math.Pi
		sp := 0.4 + rng.Float64()*0.8
		x := math.Mod(x0+math.Cos(ang)*sp*t+4*pw, pw)
		y := math.Mod(y0+math.Sin(ang)*sp*t*0.6+4*(ph-24), ph-24)
		col := []float64{60, 40, 38}
		if i%5 == 0 {
			col = []float64{170, 60, 50}
		} else if i%7 == 0 {
			col = []float64{40, 70, 120}
		}
		c.rect(int(x), int(y), int(x)+3, int(y)+3, col[0], col[1], col[2])
		c.rect(int(x)+1, int(y)+3, int(x)+2, int(y)+5, 30, 30, 30)
	}
}
