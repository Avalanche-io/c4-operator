package storeurl

import "testing"

// Local names exactly the URLs OpenWith opens as a TreeStore on this
// machine, checked against OpenWith itself for every URL in the table (a
// path form added to OpenWith is caught only once it is in the table).
func TestLocalIsThePathForms(t *testing.T) {
	dir := t.TempDir()
	for url, want := range map[string]bool{
		"file://" + dir:            true,
		dir:                        true,
		"  " + dir:                 true,
		"s3://bucket/p/":           false,
		"c4d://host:7000":          false,
		"http://host:7000":         false,
		"https://host":             false,
		"relative/path":            false,
		"":                         false,
		"FILE://" + dir:            false,
		"s3://bucket/p/?region=us": false,
	} {
		if got := Local(url); got != want {
			t.Errorf("Local(%q) = %v, want %v", url, got, want)
		}
		sh, err := OpenWith(url, nil)
		_, tree := sh.(*treeShard)
		if err == nil && tree != want {
			t.Errorf("OpenWith(%q) opens a TreeStore: %v, and Local says %v", url, tree, want)
		}
		if err != nil && want {
			t.Errorf("OpenWith(%q): %v", url, err)
		}
	}
}
