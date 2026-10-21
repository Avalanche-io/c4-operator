package huggingface

import (
	"net/url"
	"testing"
)

// From the Hub over https, a download goes only over https, to the Hub
// or the hosts its downloads use, never a look-alike.
func TestAllowed(t *testing.T) {
	c := &Client{}
	for raw, ok := range map[string]bool{
		"https://huggingface.co/x":                      true,
		"https://cdn-lfs.huggingface.co/x":              true,
		"https://cas-bridge.xethub.hf.co/x?X-Amz-Sig=1": true,
		"https://us.aws.cdn.hf.co/xet-bridge-us/x":      true,
		"http://cdn-lfs.huggingface.co/x":               false,
		"https://huggingface.co.evil.example/x":         false,
		"https://evilhuggingface.co/x":                  false,
		"https://hf.co.evil.example/x":                  false,
		"https://169.254.169.254/latest/meta-data":      false,
		"ftp://huggingface.co/x":                        false,
		"https://evil.example/huggingface.co/x":         false,
	} {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.allowed(u); (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
	}
}

// A listing's next page stays on the Hub, path prefix and all.
func TestNextPage(t *testing.T) {
	c := &Client{Endpoint: "https://mirror.example/hub"}
	got, err := c.next(`<https://mirror.example/hub/api/models/o/m/tree/abc?recursive=true&cursor=2>; rel="next"`)
	if err != nil || got != "https://mirror.example/hub/api/models/o/m/tree/abc?recursive=true&cursor=2" {
		t.Fatalf("%q %v", got, err)
	}
	got, err = c.next(`</hub/api/models/o/m/tree/abc?cursor=4>; rel="next"`)
	if err != nil || got != "https://mirror.example/hub/api/models/o/m/tree/abc?cursor=4" {
		t.Fatalf("relative: %q %v", got, err)
	}
	for _, bad := range []string{`<https://evil.example/hub/api?x=1>; rel="next"`, `<http://mirror.example/hub/api>; rel="next"`} {
		if _, err := c.next(bad); err == nil {
			t.Errorf("%s was followed", bad)
		}
	}
	if got, err := c.next(""); got != "" || err != nil {
		t.Fatalf("no link: %q %v", got, err)
	}
}
