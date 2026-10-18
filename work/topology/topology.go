// Package topology describes the farm's sites and the links between them:
// where pools run, where each project's store is, and what moving bytes
// from one site to another takes and costs.
//
// Version 1 is read from a JSON file (the HUD's --hud-sites file, which
// also accepts the older bare array of sites). The types are shaped to
// back Site and Link resources later: each object has a name and a spec
// that a CRD can carry unchanged, and the file carries the schema version.
package topology

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
)

// Version is the schema version this package reads and writes.
const Version = 1

// Topology is the farm's sites and links.
type Topology struct {
	Version int    `json:"version"`
	Sites   []Site `json:"sites"`
	Links   []Link `json:"links,omitempty"`
}

// Site is a place the farm's nodes run: a studio machine room or a cloud
// region. Nodes join a site by their topology.kubernetes.io/region label,
// and a pool by its "site" attribute.
type Site struct {
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	// Region is the topology.kubernetes.io/region of the site's nodes.
	Region string  `json:"region"`
	Lat    float64 `json:"lat"`
	Lon    float64 `json:"lon"`
	// Kind is onprem or cloud.
	Kind       string   `json:"kind"`
	Localities []string `json:"localities,omitempty"`
	// TZ is the site's IANA time zone; empty shows the viewer's.
	TZ string `json:"tz,omitempty"`
	// Regional lists the site's regional replicas (host:port): where
	// staging asks what the site holds and asks it to fetch.
	Regional []string `json:"regional,omitempty"`
	// Stores lists the projects whose authoritative store is at this site:
	// the origin of bytes another site lacks.
	Stores []string `json:"stores,omitempty"`
}

// Channels a link can be.
const (
	SameZone     = "same-zone"
	InterRegion  = "inter-region"
	Internet     = "internet"
	Interconnect = "interconnect"
	Shipment     = "shipment"
)

// Link is a path between two sites that moves bytes, shared by every
// transfer over it, in either direction.
type Link struct {
	Name    string `json:"name"`
	A       string `json:"a"`
	B       string `json:"b"`
	Channel string `json:"channel"`
	// Gbps is the link's usable throughput, in gigabits per second.
	Gbps float64 `json:"gbps"`
	// LatencySeconds is the time before the first byte lands: setup, or a
	// shipment's transit.
	LatencySeconds float64 `json:"latencySeconds,omitempty"`
	// EgressPerGB is charged by the sending end, IngressPerGB (an ingress
	// or processing fee) by the receiving end, in dollars per GB (10^9
	// bytes); FlatPerTransfer per transfer (a shipment's device fee).
	EgressPerGB     float64 `json:"egressPerGB,omitempty"`
	IngressPerGB    float64 `json:"ingressPerGB,omitempty"`
	FlatPerTransfer float64 `json:"flatPerTransfer,omitempty"`
}

// BytesPerSecond is the link's throughput.
func (l Link) BytesPerSecond() float64 { return l.Gbps * 1e9 / 8 }

// Latency is the link's latency.
func (l Link) Latency() time.Duration { return time.Duration(l.LatencySeconds * float64(time.Second)) }

// Cost is what moving bytes over the link and back bytes in the other
// direction (outputs returning) costs, in dollars.
func (l Link) Cost(bytes, back float64) float64 {
	return l.FlatPerTransfer + (bytes+back)/1e9*(l.EgressPerGB+l.IngressPerGB)
}

// Load reads a topology file.
func Load(path string) (*Topology, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	t, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return t, nil
}

// Parse reads a topology: a version 1 object, or a bare JSON array of sites
// (the HUD's older --hud-sites file).
func Parse(b []byte) (*Topology, error) {
	var t Topology
	if s := strings.TrimSpace(string(b)); strings.HasPrefix(s, "[") {
		if err := json.Unmarshal(b, &t.Sites); err != nil {
			return nil, fmt.Errorf("want a topology object or a JSON array of sites: %w", err)
		}
		t.Version = Version
	} else if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("want a topology object: %w", err)
	}
	if t.Version != Version {
		return nil, fmt.Errorf("topology version %d, this build reads %d", t.Version, Version)
	}
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Validate checks names, kinds, positions, time zones and links.
func (t *Topology) Validate() error {
	seen := map[string]bool{}
	for i, s := range t.Sites {
		switch {
		case s.Name == "":
			return fmt.Errorf("site %d has no name", i)
		case seen[s.Name]:
			return fmt.Errorf("site %s appears twice", s.Name)
		case s.Kind != "onprem" && s.Kind != "cloud":
			return fmt.Errorf("site %s: kind %q, want onprem or cloud", s.Name, s.Kind)
		case s.Lat < -90 || s.Lat > 90 || s.Lon < -180 || s.Lon > 180:
			return fmt.Errorf("site %s: lat %g lon %g is not a position", s.Name, s.Lat, s.Lon)
		}
		if s.TZ != "" {
			if _, err := time.LoadLocation(s.TZ); err != nil {
				return fmt.Errorf("site %s: tz %q is not an IANA time zone", s.Name, s.TZ)
			}
		}
		seen[s.Name] = true
	}
	names := map[string]bool{}
	for i, l := range t.Links {
		switch {
		case l.Name == "":
			return fmt.Errorf("link %d has no name", i)
		case names[l.Name]:
			return fmt.Errorf("link %s appears twice", l.Name)
		case !seen[l.A] || !seen[l.B] || l.A == l.B:
			return fmt.Errorf("link %s: %q to %q, want two different sites", l.Name, l.A, l.B)
		case l.Gbps <= 0 || math.IsInf(l.Gbps, 0):
			return fmt.Errorf("link %s: gbps %g, want above 0", l.Name, l.Gbps)
		case l.LatencySeconds < 0 || l.EgressPerGB < 0 || l.IngressPerGB < 0 || l.FlatPerTransfer < 0:
			return fmt.Errorf("link %s: negative latency or price", l.Name)
		}
		switch l.Channel {
		case SameZone, InterRegion, Internet, Interconnect, Shipment:
		default:
			return fmt.Errorf("link %s: channel %q, want %s, %s, %s, %s or %s", l.Name, l.Channel, SameZone, InterRegion, Internet, Interconnect, Shipment)
		}
		names[l.Name] = true
	}
	return nil
}

// Site returns the site named name, or nil.
func (t *Topology) Site(name string) *Site {
	for i := range t.Sites {
		if t.Sites[i].Name == name {
			return &t.Sites[i]
		}
	}
	return nil
}

// SiteOfRegion returns the site whose nodes carry region, or "".
func (t *Topology) SiteOfRegion(region string) string {
	for _, s := range t.Sites {
		if s.Region != "" && s.Region == region {
			return s.Name
		}
	}
	return ""
}

// StoreSite returns the site of a project's store, or "".
func (t *Topology) StoreSite(project string) string {
	for _, s := range t.Sites {
		for _, p := range s.Stores {
			if p == project {
				return s.Name
			}
		}
	}
	return ""
}

// Between returns the links joining sites a and b, in file order.
func (t *Topology) Between(a, b string) []Link {
	var out []Link
	for _, l := range t.Links {
		if l.A == a && l.B == b || l.A == b && l.B == a {
			out = append(out, l)
		}
	}
	return out
}
