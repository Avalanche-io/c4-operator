package capture

import "testing"

func TestControlDirIsNotOutput(t *testing.T) {
	for p, want := range map[string]bool{
		".c4/": true, ".c4": true, ".c4/progress": true,
		"render/.c4/progress": false, ".c4x/a": false, "preview/1024.png": false, "render/sm0140.1001.exr": false,
	} {
		if got := IsControl(p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}
