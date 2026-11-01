package main

import (
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
)

// --farm-book-seal is seconds, never minutes, and the book needs the farm
// CA, since every segment is signed; without --system-store nothing is
// asked of either.
func TestFarmBookSealFlagBounded(t *testing.T) {
	for _, c := range []struct {
		store   string
		every   time.Duration
		signing bool
		want    string
	}{
		{"", time.Hour, false, ""},
		{"s3://book.studio.example/c4", farmbook.DefaultSealEvery, true, ""},
		{"s3://book.studio.example/c4", 30 * time.Second, true, ""},
		{"s3://book.studio.example/c4", 31 * time.Second, true, "--farm-book-seal"},
		{"s3://book.studio.example/c4", 2 * time.Minute, true, "--farm-book-seal"},
		{"s3://book.studio.example/c4", 0, true, "--farm-book-seal"},
		{"s3://book.studio.example/c4", time.Second, false, "--signing-namespace"},
	} {
		err := checkFarmBookFlags(c.store, c.every, c.signing)
		if (err == nil) != (c.want == "") || err != nil && !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q every %s signing %v: %v, want %q", c.store, c.every, c.signing, err, c.want)
		}
	}
}
