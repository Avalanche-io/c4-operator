package order

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"sort"
	"testing"
)

func TestParseFrames(t *testing.T) {
	cases := []struct {
		in   string
		want []int
	}{
		{"1001", []int{1001}},
		{"1001-1004", []int{1001, 1002, 1003, 1004}},
		{"1001-1008x2", []int{1001, 1003, 1005, 1007}},
		{"1001-1009x4", []int{1001, 1005, 1009}},
		{" 1-3 , 7 ,10-20x5", []int{1, 2, 3, 7, 10, 15, 20}},
		{"1050,1001-1002", []int{1001, 1002, 1050}},
		{"0-0", []int{0}},
	}
	for _, c := range cases {
		got, err := ParseFrames(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %v want %v", c.in, got, c.want)
		}
	}
	long, err := ParseFrames("1001-1010,1050,1100-1120x5")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1001, 1002, 1003, 1004, 1005, 1006, 1007, 1008, 1009, 1010, 1050, 1100, 1105, 1110, 1115, 1120}
	if !reflect.DeepEqual(long, want) {
		t.Errorf("got %v", long)
	}
	n240, _ := ParseFrames("1001-1240")
	if len(n240) != 240 {
		t.Errorf("1001-1240 has %d frames", len(n240))
	}
	n120, _ := ParseFrames("1001-1240x2")
	if len(n120) != 120 || n120[119] != 1239 {
		t.Errorf("1001-1240x2: %d frames, last %d", len(n120), n120[len(n120)-1])
	}
}

func TestParseFramesErrors(t *testing.T) {
	bad := []string{
		"", " ", ",", "1,,2", "a", "1-", "-5", "5-1", "1-10x0", "1-10x", "1x2",
		"1-10x-1", "1.5", "1-3,2", "1-2000000000", "1-10xa", "--1",
	}
	for _, in := range bad {
		if _, err := ParseFrames(in); err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
}

func TestChunk(t *testing.T) {
	f, _ := ParseFrames("1-10")
	c := Chunk(f, 4)
	if len(c) != 3 || !reflect.DeepEqual(c[2], []int{9, 10}) {
		t.Fatalf("got %v", c)
	}
	if got := Chunk(f, 0); len(got) != 10 {
		t.Fatalf("size 0: %d units", len(got))
	}
	// Appending to one chunk must not clobber the next.
	c[0] = append(c[0], 99)
	if c[1][0] != 5 {
		t.Fatalf("chunks alias: %v", c)
	}
}

func TestSetRoundTrip(t *testing.T) {
	cases := map[string][]int{
		"":                 nil,
		"0":                {0},
		"0-99,102,104-180": append(append(SequentialOrder(100), 102), seq(104, 180)...),
		"3,5,7":            {7, 5, 3, 5},
		"0-4":              {4, 3, 2, 1, 0, 2},
	}
	for want, in := range cases {
		got := EncodeSet(in)
		if got != want {
			t.Errorf("EncodeSet(%v) = %q want %q", in, got, want)
		}
	}
	r := rand.New(rand.NewPCG(1, 2))
	for trial := 0; trial < 2000; trial++ {
		n := r.IntN(300)
		var in []int
		for i := 0; i < n; i++ {
			if r.Float64() < 0.6 {
				in = append(in, i)
			}
		}
		r.Shuffle(len(in), func(a, b int) { in[a], in[b] = in[b], in[a] })
		enc := EncodeSet(in)
		dec, err := DecodeSet(enc)
		if err != nil {
			t.Fatalf("decode %q: %v", enc, err)
		}
		want := append([]int(nil), in...)
		sort.Ints(want)
		if len(want) == 0 {
			want = []int{}
		}
		if !reflect.DeepEqual(dec, want) {
			t.Fatalf("round trip %v -> %q -> %v", want, enc, dec)
		}
		cnt, _ := CountSet(enc)
		if cnt != len(want) {
			t.Fatalf("count %d want %d", cnt, len(want))
		}
		if EncodeSet(dec) != enc {
			t.Fatalf("not canonical: %q", enc)
		}
	}
}

func TestDecodeSetErrors(t *testing.T) {
	for _, in := range []string{"5,3", "0-10,5", "4-2", "a", "1,", ",1", "1--2", "-1", "0-3,3"} {
		if _, err := DecodeSet(in); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
	got, err := DecodeSet("0-4,5-9")
	if err != nil || len(got) != 10 {
		t.Errorf("adjacent runs: %v %v", got, err)
	}
}

func seq(lo, hi int) []int {
	var out []int
	for i := lo; i <= hi; i++ {
		out = append(out, i)
	}
	return out
}

func TestBinarySmall(t *testing.T) {
	cases := map[int][]int{
		0: {},
		1: {0},
		2: {0, 1},
		3: {0, 2, 1},
		5: {0, 4, 2, 1, 3},
		9: {0, 8, 4, 2, 6, 1, 5, 3, 7},
	}
	for n, want := range cases {
		if got := BinaryOrder(n); !reflect.DeepEqual(got, want) {
			t.Errorf("n=%d got %v want %v", n, got, want)
		}
	}
	// 17 units: every level is exact, van der Corput within each level.
	got := BinaryOrder(17)
	want := []int{0, 16, 8, 4, 12, 2, 10, 6, 14, 1, 9, 5, 13, 3, 11, 7, 15}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("n=17 got %v", got)
	}
}

// TestBinaryProperties checks, for every n from 1 to 2000:
//  1. the order is a permutation of 0..n-1 and the defensive sweep in
//     binaryOrder appended nothing (the level construction alone covers
//     every index);
//  2. the first two units are 0 and n-1;
//  3. for every k with 2^k <= n-1, the first 2^k+1 units are exactly
//     {round(m*(n-1)/2^k) : m = 0..2^k} and their largest consecutive gap
//     is at most ceil((n-1)/2^k);
//  4. for every prefix length p >= 2, with k = floor(log2(p-1)) capped so
//     that 2^k <= n-1, the prefix's largest gap is at most
//     ceil((n-1)/2^k). This follows from 3 because a longer prefix only
//     adds indexes; it is also checked directly for n <= 300.
func TestBinaryProperties(t *testing.T) {
	for n := 1; n <= 2000; n++ {
		ord, swept := binaryOrder(n)
		if swept != 0 {
			t.Fatalf("n=%d: sweep appended %d indexes", n, swept)
		}
		if len(ord) != n {
			t.Fatalf("n=%d: length %d", n, len(ord))
		}
		s := append([]int(nil), ord...)
		sort.Ints(s)
		for i, v := range s {
			if v != i {
				t.Fatalf("n=%d: not a permutation", n)
			}
		}
		if n >= 2 && (ord[0] != 0 || ord[1] != n-1) {
			t.Fatalf("n=%d: starts %v", n, ord[:2])
		}
		if n < 3 {
			continue
		}
		span := n - 1
		for k := 0; 1<<k <= span; k++ {
			p := 1<<k + 1
			want := map[int]bool{}
			for m := 0; m <= 1<<k; m++ {
				want[roundHalfUp(m*span, 1<<k)] = true
			}
			if len(want) != p {
				t.Fatalf("n=%d k=%d: level points not distinct", n, k)
			}
			for _, v := range ord[:p] {
				if !want[v] {
					t.Fatalf("n=%d k=%d: prefix has %d, not a level point", n, k, v)
				}
			}
			bound := (span + (1 << k) - 1) / (1 << k)
			if g := MaxGap(ord[:p]); g > bound {
				t.Fatalf("n=%d k=%d: gap %d > %d", n, k, g, bound)
			}
		}
		if n > 300 {
			continue // property 4 is implied by 3; checked directly for small n only, for speed
		}
		for p := 2; p <= n; p++ {
			k := 0
			for 1<<(k+1) <= p-1 && 1<<(k+1) <= span {
				k++
			}
			bound := (span + (1 << k) - 1) / (1 << k)
			if g := MaxGap(ord[:p]); g > bound {
				t.Fatalf("n=%d p=%d: gap %d > %d", n, p, g, bound)
			}
		}
	}
}

func roundHalfUp(num, den int) int { return (2*num + den) / (2 * den) }

func TestCoarsePrefix(t *testing.T) {
	// DESIGN 9.8: after 33 units of a 240-frame shot, every 8th frame exists.
	if got := CoarsePrefix(240, 8); got != 33 {
		t.Fatalf("CoarsePrefix(240,8) = %d", got)
	}
	if g := MaxGap(BinaryOrder(240)[:33]); g > 8 {
		t.Fatalf("gap after 33 of 240: %d", g)
	}
	for n := 1; n <= 600; n++ {
		for _, every := range []int{1, 2, 3, 8, 16} {
			p := CoarsePrefix(n, every)
			if p > n || p < min(n, 2) {
				t.Fatalf("n=%d every=%d: prefix %d", n, every, p)
			}
			if n >= 2 && MaxGap(BinaryOrder(n)[:p]) > every {
				t.Fatalf("n=%d every=%d: gap %d after %d", n, every, MaxGap(BinaryOrder(n)[:p]), p)
			}
		}
	}
}

func TestOtherOrders(t *testing.T) {
	if got := ReverseOrder(4); !reflect.DeepEqual(got, []int{3, 2, 1, 0}) {
		t.Fatal(got)
	}
	got := FailRateOrder([]float64{0.1, 0.5, 0.1, 0.9})
	if !reflect.DeepEqual(got, []int{3, 1, 0, 2}) {
		t.Fatal(got)
	}
	for _, k := range []Kind{Sequential, Reverse, Binary} {
		o, err := Units(k, 50, nil)
		if err != nil || len(o) != 50 {
			t.Fatal(k, err)
		}
		s := slices.Clone(o)
		sort.Ints(s)
		if !reflect.DeepEqual(s, SequentialOrder(50)) {
			t.Fatal(k, "not a permutation")
		}
	}
	if _, err := Units(FailRate, 3, []float64{1}); err == nil {
		t.Fatal("score length mismatch accepted")
	}
	if _, err := Units("Spiral", 3, nil); err == nil {
		t.Fatal("unknown kind accepted")
	}
}
