package needs

import "testing"

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
