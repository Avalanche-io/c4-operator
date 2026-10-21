package farmcli

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// traceRecord is what the farm says of a file of a job's output: the
// signed record of the unit that wrote it, and whether it verifies.
type traceRecord struct {
	Verified bool   `json:"verified"`
	Signer   string `json:"signer"`
	SignedAt string `json:"signedAt"`
	Record   string `json:"record"`
	File     struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	} `json:"file"`
	Lineage   []traceInput `json:"lineage"`
	Statement struct {
		Predicate struct {
			Invocation struct {
				Job, Namespace, Unit, Attempt string
			} `json:"invocation"`
			Runtime struct {
				Node   string `json:"node"`
				Images []struct {
					Digest string `json:"digest"`
				} `json:"images"`
			} `json:"runtime"`
		} `json:"predicate"`
	} `json:"statement"`
}

// traceInput is a tree the unit read: an upload, or a job's output.
type traceInput struct {
	View    string `json:"view"`
	Job     string `json:"job"`
	FromJob string `json:"fromJob"`
}

// Trace is `c4 farm trace JOB FILE`: the file's signed record, whether it
// verifies against the farm's signing roots, the unit and node that wrote
// it, and the trees that unit read.
func Trace(ctx context.Context, c *Client, o Out, project, ref, path string) error {
	j, err := findJob(ctx, c, project, ref)
	if err != nil {
		return err
	}
	var r traceRecord
	p := "/trace/" + url.PathEscape(j.Namespace) + "/" + url.PathEscape(j.Name) + "?" + url.Values{"path": {path}}.Encode()
	if err := c.Do(ctx, "GET", p, nil, &r); err != nil {
		return err
	}
	verdict := "verifies"
	if !r.Verified {
		verdict = "DOES NOT VERIFY against the farm's signing roots"
	}
	o.printf("%s   %s   %s\n", r.File.Path, shortID(r.File.ID), verdict)
	inv, rt := r.Statement.Predicate.Invocation, r.Statement.Predicate.Runtime
	by := r.Signer
	if i := strings.Index(by, "/id/node/"); i >= 0 {
		by = "the node"
	}
	made := []string{j.Namespace + "/" + j.Name}
	if inv.Unit != "" {
		made = append(made, "unit "+inv.Unit)
	}
	if rt.Node != "" {
		made = append(made, "node "+rt.Node)
	}
	made = append(made, "signed by "+by)
	o.printf("  made by   %s\n", strings.Join(made, " · "))
	if t, err := time.Parse(time.RFC3339Nano, r.SignedAt); err == nil {
		o.printf("  signed    %s · record %s\n", o.Clock.AtUTC(t), shortID(r.Record))
	}
	for _, in := range r.Lineage {
		switch from := in.Job + in.FromJob; {
		case from != "":
			o.printf("  read      the output of %s · view %s\n", from, shortID(in.View))
		default:
			o.printf("  read      view %s, as uploaded\n", shortID(in.View))
		}
	}
	for _, im := range rt.Images {
		o.printf("  image     %s\n", im.Digest)
	}
	if !r.Verified {
		return fmt.Errorf("%s: its record does not verify", r.File.Path)
	}
	return nil
}
