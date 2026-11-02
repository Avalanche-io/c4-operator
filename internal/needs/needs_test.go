package needs

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestAbout(t *testing.T) {
	got, err := About([]string{"unit:schema", " plates/ ", "", "path:render/beauty", "job:show/plates"})
	want := []string{"unit:schema", "path:plates/", "path:render/beauty", "job:show/plates"}
	if err != nil || len(got) != len(want) {
		t.Fatalf("%v %v", got, err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("%d: %q, want %q", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"shot:sm0140", "unit:", "job:plates", "path:a,b"} {
		if _, err := About([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if m := Message(" the frame rate ", "24 or 25", want[:1]); m != "the frame rate\nwhy: 24 or 25\nabout: unit:schema" {
		t.Errorf("message %q", m)
	}
	if m := Message("the frame rate", "", nil); m != "the frame rate" {
		t.Errorf("message %q", m)
	}
}

// A question fits a termination message: the why is cut first, then the
// text, at a character boundary and marked; a short one is left alone.
func TestFit(t *testing.T) {
	about := []string{"unit:schema", "path:plates/"}
	if w, y, ab := Fit(" the frame rate ", "the brief gives 24 and 25", about); w != "the frame rate" || y != "the brief gives 24 and 25" || len(ab) != 2 {
		t.Fatalf("a short question changed: %q %q %v", w, y, ab)
	}
	long := strings.Repeat("é", 3000) // two bytes each
	w, y, ab := Fit("the delivery frame rate", long, about)
	msg := Message(w, y, ab)
	if len(msg) > MessageMax || w != "the delivery frame rate" || !strings.HasSuffix(y, CutMark) || !utf8.ValidString(msg) || len(ab) != 2 {
		t.Fatalf("a long why: %d bytes, text %q, why ends %q, valid %v, about %v", len(msg), w, y[max(0, len(y)-12):], utf8.ValidString(msg), ab)
	}
	w, y, _ = Fit(long, long, about)
	if msg := Message(w, y, about); len(msg) > MessageMax || !strings.HasSuffix(w, CutMark) || !strings.HasSuffix(y, CutMark) {
		t.Fatalf("a long text and why: %d bytes", len(msg))
	}
}
