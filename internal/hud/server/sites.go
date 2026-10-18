package server

import (
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/work/topology"
)

// LoadSites reads the farm's sites for the HUD's map (--hud-sites): a
// topology file (work/topology: sites and the links between them), or the
// older bare JSON array of { name, label, region, lat, lon, kind,
// localities, tz }. Nodes join a site by their topology.kubernetes.io/region
// label, and a site's localities are the node localities (racks, zones) it
// holds.
func LoadSites(path string) ([]api.Site, error) {
	t, err := topology.Load(path)
	if err != nil {
		return nil, err
	}
	sites := make([]api.Site, 0, len(t.Sites))
	for _, s := range t.Sites {
		as := api.Site{Name: s.Name, Label: s.Label, Region: s.Region, Lat: s.Lat, Lon: s.Lon, Kind: s.Kind, Localities: s.Localities, TZ: s.TZ}
		if as.Label == "" {
			as.Label = as.Name
		}
		if as.Localities == nil {
			as.Localities = []string{}
		}
		sites = append(sites, as)
	}
	return sites, nil
}
