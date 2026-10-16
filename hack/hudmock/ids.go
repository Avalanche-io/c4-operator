package main

import (
	"crypto/sha512"
	"math/big"
	"sort"
	"strconv"
	"strings"
)

const base58 = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// c4id returns the C4 ID of b: the SHA-512 digest in base58, padded to 88
// characters, with the "c4" prefix.
func c4id(b []byte) string {
	sum := sha512.Sum512(b)
	n := new(big.Int).SetBytes(sum[:])
	out := make([]byte, 88)
	for i := range out {
		out[i] = '1'
	}
	base := big.NewInt(58)
	mod := new(big.Int)
	for i := 87; i >= 0 && n.Sign() > 0; i-- {
		n.DivMod(n, base, mod)
		out[i] = base58[mod.Int64()]
	}
	return "c4" + string(out)
}

// fakeID is a stable C4 ID for a name, used for views and changesets the
// mock does not actually hold.
func fakeID(parts ...string) string {
	return c4id([]byte(strings.Join(parts, "\x00")))
}

// rle encodes a set of unit indexes as "0-99,102".
func rle(idx []int) string {
	if len(idx) == 0 {
		return ""
	}
	sort.Ints(idx)
	var b strings.Builder
	start, prev := idx[0], idx[0]
	flush := func() {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(start))
		if prev != start {
			b.WriteByte('-')
			b.WriteString(strconv.Itoa(prev))
		}
	}
	for _, v := range idx[1:] {
		if v == prev+1 {
			prev = v
			continue
		}
		flush()
		start, prev = v, v
	}
	flush()
	return b.String()
}

// parseSet parses "0-99,102" back into indexes.
func parseSet(s string) []int {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, found := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if found {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		for i := a; i <= b && i-a < 100000; i++ {
			out = append(out, i)
		}
	}
	return out
}

// vdc is the van der Corput radical inverse of i in base 2.
func vdc(i int) float64 {
	f, denom := 0.0, 1.0
	for i > 0 {
		denom *= 2
		f += float64(i&1) / denom
		i >>= 1
	}
	return f
}

// binaryOrder returns the positions 0..n-1 in binary order: first and last,
// then the midpoint, the quarters, the eighths and so on, each fraction
// mapped to the nearest position and positions already taken skipped.
func binaryOrder(n int) []int {
	if n <= 0 {
		return nil
	}
	taken := make([]bool, n)
	out := make([]int, 0, n)
	add := func(p int) {
		if p < 0 || p >= n || taken[p] {
			return
		}
		taken[p] = true
		out = append(out, p)
	}
	add(0)
	add(n - 1)
	limit := 1
	for limit < n {
		limit <<= 1
	}
	for i := 1; len(out) < n && i < limit*4; i++ {
		add(int(vdc(i)*float64(n-1) + 0.5))
	}
	for p := 0; p < n; p++ {
		add(p)
	}
	return out
}
