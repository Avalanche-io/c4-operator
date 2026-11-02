package agent

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/needs"
	"github.com/Avalanche-io/c4-operator/internal/provenance"
)

// A block in a pod signs what it reports about itself as its unit, as
// c4-unit does: its needs, cut to fit and signed exactly as written, and a
// re-check's withdrawal.
func TestPodBlockSignsItsReports(t *testing.T) {
	ctx := context.Background()
	ca, err := provenance.NewCA(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.Cert)
	const jobUID = "9c1d2e7f-0000-4000-8000-000000000001"
	issue := provenance.LocalIssuer(ca, provenance.UnitIdentity("show-a", "port", "2", jobUID, 1), time.Hour)
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(fakePrint), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, control := t.TempDir(), t.TempDir()
	term := filepath.Join(t.TempDir(), "termination-log")
	t.Setenv("FAKE_SLUG", ProjectSlug(cwd))
	t.Setenv("FAKE_LOG", filepath.Join(t.TempDir(), "calls"))
	for k, v := range map[string]string{"C4_JOB": "port", "C4_UNIT_NAME": "notes", "C4_TERMINATION_LOG": term, "C4_CONTROL": control,
		"C4_NAMESPACE": "show-a", "C4_JOB_UID": jobUID, "C4_POD": "port-u2", "C4_POD_UID": "5e1f0c2a-0000-4000-8000-0000000000a2",
		"C4_SPEC": strings.Repeat("ab", 32), "C4_OWNER": "dana"} {
		t.Setenv(k, v)
	}
	os.WriteFile(filepath.Join(control, "acts.seen"), []byte("1"), 0o644)
	acts := func(out string) []map[string]any {
		t.Helper()
		var raw []json.RawMessage
		json.Unmarshal(mustRead(t, filepath.Join(out, ".c4", "acts")), &raw)
		var got []map[string]any
		for _, r := range raw {
			_, a, err := provenance.VerifyUnitReport(r, roots, time.Now())
			if err != nil {
				t.Fatalf("an act does not verify: %v", err)
			}
			p := map[string]any{}
			json.Unmarshal(a.Predicate.Params, &p)
			p["act"] = a.Predicate.Act
			got = append(got, p)
		}
		return got
	}
	run := func(out string) int {
		t.Helper()
		t.Setenv("C4_OUTPUT", out)
		b := &PodBlock{Out: out, ConfigDir: t.TempDir(), Cwd: cwd, Claude: bin, HookCommand: "true", Env: os.Getenv, Logf: t.Logf, Issue: issue}
		code, err := b.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return code
	}

	why := strings.Repeat("the brief gives 24 here and 25 there. ", 200)
	b, _ := json.Marshal(map[string]any{"needs": "the delivery frame rate", "why": why, "about": []string{"unit:schema"}})
	t.Setenv("FAKE_NEEDS", string(b))
	outA := t.TempDir()
	if code := run(outA); code != PodBlockNeedsInformation {
		t.Fatalf("exit %d", code)
	}
	msg := string(mustRead(t, term))
	text, y, about := needs.Parse(msg)
	got := acts(outA)
	if len(msg) > needs.MessageMax || len(got) != 1 || got[0]["act"] != "needs" || got[0]["what"] != text || got[0]["why"] != y ||
		len(about) != 1 || !strings.HasSuffix(y, needs.CutMark) {
		t.Fatalf("message %d bytes; acts %v", len(msg), got)
	}

	// its re-check, from the same view, withdraws, signed
	os.WriteFile(filepath.Join(control, "acts.seen"), []byte("2"), 0o644)
	t.Setenv(EnvRecheck, "1")
	t.Setenv("C4_NEEDED", msg)
	t.Setenv(EnvRecheckWhy, "unit schema succeeded")
	t.Setenv("FAKE_NEEDS", "")
	t.Setenv("FAKE_WITHDRAW", `{"why": "schema fixes 24 fps"}`)
	if code := run(outA); code != PodBlockDone || string(mustRead(t, term)) != "withdrawn: schema fixes 24 fps" {
		t.Fatalf("withdraw: exit %d, %q", code, mustRead(t, term))
	}
	if got := acts(outA); len(got) != 2 || got[1]["act"] != "withdraw" || got[1]["why"] != "schema fixes 24 fps" {
		t.Fatalf("withdraw acts %v", got)
	}
}
