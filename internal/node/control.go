package node

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Avalanche-io/c4"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
)

// Documents a unit leaves for the farm in its control directory (.c4/),
// which capture never includes in its output. At capture the daemon
// stores each one present in the project's store and names it on the pod,
// where the queue reads it: the expansion document (DESIGN 10.6) and a
// review unit's verdict.
var controlDocs = []struct {
	file, annotation string
}{
	{"expand.json", c4v1.AnnotationOutputExpand},
	{"verdict.json", c4v1.AnnotationOutputVerdict},
}

// maxControlDoc bounds a control document, in bytes.
const maxControlDoc = 16 << 20

// storeControlDocs stores the control documents in upper and returns the
// annotations that name them.
func storeControlDocs(ctx context.Context, sink captureSink, project, upper string) (map[string]string, error) {
	out := map[string]string{}
	for _, cd := range controlDocs {
		p := filepath.Join(upper, ".c4", cd.file)
		f, err := os.Open(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(io.LimitReader(f, maxControlDoc+1))
		f.Close()
		if err != nil {
			return nil, err
		}
		if len(data) > maxControlDoc {
			return nil, fmt.Errorf(".c4/%s is over %d bytes", cd.file, maxControlDoc)
		}
		id := c4.Identify(bytes.NewReader(data))
		if err := sink.PutBytes(ctx, project, id, data); err != nil {
			return nil, fmt.Errorf("store .c4/%s: %w", cd.file, err)
		}
		out[cd.annotation] = id.String()
	}
	return out, nil
}
