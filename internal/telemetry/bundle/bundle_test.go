package bundle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/telemetry"
	"github.com/Avalanche-io/c4-operator/internal/telemetry/canary"
	"github.com/Avalanche-io/c4-operator/work/sched"
)

func testKey(t *testing.T) telemetry.Key {
	t.Helper()
	b := make([]byte, telemetry.KeySize)
	for i := range b {
		b[i] = byte(31 + i)
	}
	k, err := telemetry.NewKey(b, 3)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// snapshots are three ticks of the canary farm, redacted by one redactor
// as a farm's ring redacts them. The money factor is fixed so the bundle's
// bytes are the same on every run.
func snapshots(t *testing.T) []telemetry.Snapshot {
	t.Helper()
	r := &telemetry.Redactor{Key: testKey(t), MoneyFactor: 1.37, Policy: telemetry.PolicyObjective}
	var out []telemetry.Snapshot
	for i := 0; i < 3; i++ {
		s, st, cfg := canary.Farm()
		s.Now = s.Now.Add(time.Duration(i) * time.Minute)
		j := &s.Jobs[len(s.Jobs)-1]
		j.Runnable = j.Runnable[:len(j.Runnable)-i]
		j.RunnableTotal = len(j.Runnable)
		before := st.Doc()
		d := sched.Decide(s, st, cfg)
		rec, _ := r.Redact(s, sched.StateFrom(before), cfg, d)
		out = append(out, rec)
	}
	return out
}

type entry struct {
	name string
	data []byte
}

func untar(t *testing.T, b []byte) []entry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if !gz.ModTime.IsZero() || gz.Name != "" || gz.Comment != "" {
		t.Fatalf("the gzip header carries a time or a name: %+v", gz.Header)
	}
	var out []entry
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.ModTime.Unix() != 0 || h.Uname != "" || h.Gname != "" || h.Uid != 0 || h.Gid != 0 {
			t.Fatalf("%s's tar header carries a time or an owner: %+v", h.Name, h)
		}
		data, _ := io.ReadAll(tr)
		out = append(out, entry{h.Name, data})
	}
}

// A bundle reads back as written: README.txt first, then the manifest;
// the ID is the manifest's; every snapshot replays, after decoding, to
// the farm's own decision hash; and the same snapshots make the same
// bytes, so nothing in the bundle depends on when or where it was written.
func TestBundleRoundTrip(t *testing.T) {
	snaps := snapshots(t)
	var buf bytes.Buffer
	id, err := Write(&buf, Bundle{Snapshots: snaps})
	if err != nil {
		t.Fatal(err)
	}
	c, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if c.ID != id {
		t.Fatalf("read back as %s, written as %s", c.ID, id)
	}
	if len(c.Snapshots) != len(snaps) {
		t.Fatalf("%d snapshots read back, %d written", len(c.Snapshots), len(snaps))
	}
	for i, s := range c.Snapshots {
		if !s.Faithful {
			t.Errorf("snapshot %d does not replay: %+v", i, s.Divergences)
		}
		if got := telemetry.ReplayHash(s, s.Policy.Func()); got != s.DecisionHash || s.DecisionHash != snaps[i].DecisionHash {
			t.Errorf("snapshot %d, decoded, replays to another decision", i)
		}
	}
	if !c.Redaction.MoneyScaled || !c.Redaction.BytesTwoFigures || len(c.Redaction.Snapshots) != 3 || c.Redaction.KeyEpoch != 3 {
		t.Errorf("redaction.json: %+v", c.Redaction)
	}
	if len(c.Farm.Pools) != 2 || c.Farm.Jobs != 4 {
		t.Errorf("farm.json: %+v", c.Farm)
	}
	if !strings.Contains(c.README, "Snapshots: 3, of which 3 replay") {
		t.Errorf("README.txt:\n%s", c.README)
	}
	es := untar(t, buf.Bytes())
	var names []string
	for _, e := range es {
		names = append(names, e.name)
	}
	want := "README.txt manifest.c4m snapshots/000.json snapshots/001.json snapshots/002.json build.json farm.json redaction.json events.jsonl counters.jsonl"
	if got := strings.Join(names, " "); got != want {
		t.Errorf("entries %s\nwant    %s", got, want)
	}
	if FileName(id) != "c4-diagnose-"+id.String()[:12]+".tgz" {
		t.Errorf("file name %s", FileName(id))
	}
	var again bytes.Buffer
	if _, err := Write(&again, Bundle{Snapshots: snapshots(t)}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), again.Bytes()) {
		t.Error("the same snapshots made a different bundle")
	}
}

// No project detail of the canary farm is anywhere in the bundle, in any
// encoding: the records are scanned as a single record is, down to four-
// character runs of a name; README.txt and the manifest, which telemetry
// writes from fixed words, file names and C4 IDs of the records, are
// scanned for whole tokens. No entry holds a wall-clock time of the farm.
func TestBundleCanary(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Write(&buf, Bundle{Snapshots: snapshots(t), Dropped: true}); err != nil {
		t.Fatal(err)
	}
	tokens := canary.Tokens()
	for _, e := range untar(t, buf.Bytes()) {
		var found []string
		if strings.HasSuffix(e.name, ".json") {
			skip := map[string]bool{}
			canary.KeyRuns(skip, e.data)
			for _, table := range telemetry.EnumTables() {
				for _, s := range table {
					canary.AddRuns(skip, s)
				}
			}
			found = canary.Scan(e.data, tokens, skip)
		} else {
			found = canary.ScanWhole(e.data, tokens)
		}
		if len(found) > 0 {
			t.Errorf("%s holds project details:\n%s", e.name, strings.Join(found, "\n"))
		}
		for _, tm := range []time.Time{canary.Now, canary.Deadline} {
			for _, f := range []string{tm.Format("2006-01-02"), strconv.FormatInt(tm.Unix(), 10), strconv.FormatInt(tm.UnixMilli(), 10)} {
				if bytes.Contains(e.data, []byte(f)) {
					t.Errorf("%s holds a wall-clock time: %s", e.name, f)
				}
			}
		}
	}
}

// retar rebuilds a bundle with change applied to its entries.
func retar(t *testing.T, b []byte, change func([]entry) []entry) []byte {
	t.Helper()
	es := change(untar(t, b))
	var out bytes.Buffer
	gz := gzip.NewWriter(&out)
	tw := tar.NewWriter(gz)
	for _, e := range es {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o444, Size: int64(len(e.data))}); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(e.data)
	}
	_ = tw.Close()
	_ = gz.Close()
	return out.Bytes()
}

// A bundle whose entries do not match its manifest, or that lacks one, or
// whose README names another bundle, does not read: a replay reads only
// the bundle that was reviewed.
func TestBundleRefusesTampering(t *testing.T) {
	var buf bytes.Buffer
	if _, err := Write(&buf, Bundle{Snapshots: snapshots(t)}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func([]entry) []entry{
		"an edited snapshot": func(es []entry) []entry {
			for i := range es {
				if es[i].name == "snapshots/001.json" {
					es[i].data = bytes.Replace(es[i].data, []byte(`"Faithful":true`), []byte(`"Faithful":false`), 1)
				}
			}
			return es
		},
		"a missing entry": func(es []entry) []entry {
			var out []entry
			for _, e := range es {
				if e.name != "farm.json" {
					out = append(out, e)
				}
			}
			return out
		},
		"another bundle's README": func(es []entry) []entry {
			es[0].data = []byte("C4 diagnostic bundle c4other\n")
			return es
		},
	}
	for name, change := range cases {
		if _, err := Read(bytes.NewReader(retar(t, buf.Bytes(), change))); err == nil {
			t.Errorf("%s read without complaint", name)
		}
	}
}

// A snapshot decided by a policy this build does not know is written, and
// said not to replay, rather than replayed through the wrong policy.
func TestBundleUnknownPolicy(t *testing.T) {
	snaps := snapshots(t)[:1]
	snaps[0].Policy = telemetry.PolicyOther
	var buf bytes.Buffer
	if _, err := Write(&buf, Bundle{Snapshots: snaps}); err != nil {
		t.Fatal(err)
	}
	c, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if c.Snapshots[0].Faithful || !strings.Contains(c.README, "Snapshots: 1, of which 0 replay") {
		t.Fatalf("a snapshot of an unknown policy was said to replay:\n%s", c.README)
	}
}
