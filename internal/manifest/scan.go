package manifest

import (
	"github.com/Avalanche-io/c4"
	"github.com/Avalanche-io/c4/scan"
)

// ScanContentID scans dir with the c4 scanner in content mode and returns
// the tree's content-level ID. A pod runs this against its mount to check
// it was given exactly the tree in status.viewID.
func ScanContentID(dir string) (c4.ID, error) {
	m, err := scan.Dir(dir, scan.WithMode(scan.ModeContent), scan.WithHidden(true))
	if err != nil {
		return c4.ID{}, err
	}
	return m.ComputeC4ID(), nil
}
