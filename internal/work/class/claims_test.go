package class

import "testing"

func TestClaimConflicts(t *testing.T) {
	ex := func(on string) Claim { c, _ := ParseClaim(on, ""); return c }
	sh := func(on string) Claim { c, _ := ParseClaim(on, ClaimShared); return c }
	for _, c := range []struct {
		a, b Claim
		want bool
	}{
		{ex("src/net/"), ex("src/net/conn.go"), true},
		{ex("src/net"), ex("src/net/conn.go"), true},
		{ex("./src/net/conn.go"), ex("src/net/conn.go"), true},
		{ex("src/net"), ex("src/netx"), false},
		{ex("src/a.go"), ex("src/b.go"), false},
		{ex("/"), ex("anything/at/all"), true},
		{sh("src/net/"), sh("src/net/conn.go"), false},
		{sh("src/net/"), ex("src/net/conn.go"), true},
		{ex("db:test"), ex("db:test"), true},
		{ex("db:test"), ex("db:prod"), false},
		{ex("db:test"), ex("db/test"), false},
	} {
		if got := c.a.Conflicts(c.b); got != c.want || c.b.Conflicts(c.a) != c.want {
			t.Errorf("%+v vs %+v: %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if _, err := ParseClaim("x", "sometimes"); err == nil {
		t.Fatal("a bad mode")
	}
	if _, err := ParseClaim(" ", ""); err == nil {
		t.Fatal("a claim on nothing")
	}
}
