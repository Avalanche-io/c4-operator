package server

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/Avalanche-io/c4-operator/internal/hud/api"
)

// LoadSites reads the farm's sites for the HUD's map (--hud-sites): a JSON
// array of { name, label, region, lat, lon, kind, localities }. Nodes join
// a site by their topology.kubernetes.io/region label, and a site's
// localities are the node localities (racks, zones) it holds.
func LoadSites(path string) ([]api.Site, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sites []api.Site
	if err := json.Unmarshal(b, &sites); err != nil {
		return nil, fmt.Errorf("%s: want a JSON array of sites: %w", path, err)
	}
	seen := map[string]bool{}
	for i := range sites {
		s := &sites[i]
		switch {
		case s.Name == "":
			return nil, fmt.Errorf("%s: site %d has no name", path, i)
		case seen[s.Name]:
			return nil, fmt.Errorf("%s: site %s appears twice", path, s.Name)
		case s.Kind != "onprem" && s.Kind != "cloud":
			return nil, fmt.Errorf("%s: site %s: kind %q, want onprem or cloud", path, s.Name, s.Kind)
		case s.Lat < -90 || s.Lat > 90 || s.Lon < -180 || s.Lon > 180:
			return nil, fmt.Errorf("%s: site %s: lat %g lon %g is not a position", path, s.Name, s.Lat, s.Lon)
		}
		seen[s.Name] = true
		if s.Label == "" {
			s.Label = s.Name
		}
		if s.Localities == nil {
			s.Localities = []string{}
		}
	}
	return sites, nil
}
