package storeurl

import "testing"

// Local names exactly the URLs OpenWith opens as a TreeStore on this
// machine.
func TestLocalIsThePathForms(t *testing.T) {
	for url, want := range map[string]bool{
		"file:///var/store": true,
		"/var/store":        true,
		"  /var/store":      true,
		"s3://bucket/p/":    false,
		"c4d://host:7000":   false,
		"http://host:7000":  false,
		"https://host":      false,
		"relative/path":     false,
		"":                  false,
	} {
		if got := Local(url); got != want {
			t.Errorf("Local(%q) = %v, want %v", url, got, want)
		}
	}
}
