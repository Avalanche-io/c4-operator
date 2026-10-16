// Package membership tells a node daemon who its peers are and where the
// regional tier is.
//
// Peers are the nodes of the daemon's locality (a rack or zone label),
// read from a watch on Nodes with that label selector, so the watch is
// bounded by the locality's size. The regional tier is a Service; its
// members are the ready endpoints of its EndpointSlices. Every daemon
// reads the same objects, so every daemon computes the same rendezvous
// homes without coordination. Failures do not change membership: a peer
// that is down is skipped by circuit breakers, so homes stay stable while
// it restarts.
package membership

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Member is one peer daemon or regional replica.
type Member struct {
	Name string
	Addr string // host:port
}

// Members reads membership from a watch cache.
type Members struct {
	Cache       client.Reader
	LocalityKey string // node label that defines the locality
	Locality    string // this node's value
	PeerPort    int

	RegionalNamespace string
	RegionalService   string
	RegionalPort      int // 0: the EndpointSlice port

	TTL time.Duration // how long a computed list is reused (default 2 s)

	mu       sync.Mutex
	peers    []Member
	peersAt  time.Time
	regional []Member
	regAt    time.Time
}

func (m *Members) ttl() time.Duration {
	if m.TTL <= 0 {
		return 2 * time.Second
	}
	return m.TTL
}

// Peers returns the daemons of this locality, sorted by name.
func (m *Members) Peers(ctx context.Context) ([]Member, error) {
	m.mu.Lock()
	if time.Since(m.peersAt) < m.ttl() {
		out := m.peers
		m.mu.Unlock()
		return out, nil
	}
	m.mu.Unlock()
	var nodes corev1.NodeList
	if err := m.Cache.List(ctx, &nodes, client.MatchingLabels{m.LocalityKey: m.Locality}); err != nil {
		return nil, err
	}
	var out []Member
	for _, n := range nodes.Items {
		addr := internalIP(&n)
		if addr == "" {
			continue
		}
		out = append(out, Member{Name: n.Name, Addr: addr + ":" + strconv.Itoa(m.PeerPort)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	m.mu.Lock()
	m.peers, m.peersAt = out, time.Now()
	m.mu.Unlock()
	return out, nil
}

// Regional returns the ready regional replicas, sorted by name.
func (m *Members) Regional(ctx context.Context) ([]Member, error) {
	m.mu.Lock()
	if time.Since(m.regAt) < m.ttl() {
		out := m.regional
		m.mu.Unlock()
		return out, nil
	}
	m.mu.Unlock()
	var slices discoveryv1.EndpointSliceList
	err := m.Cache.List(ctx, &slices, client.InNamespace(m.RegionalNamespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: m.RegionalService})
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []Member
	for _, s := range slices.Items {
		port := m.RegionalPort
		if port == 0 && len(s.Ports) > 0 && s.Ports[0].Port != nil {
			port = int(*s.Ports[0].Port)
		}
		for _, ep := range s.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			if len(ep.Addresses) == 0 {
				continue
			}
			name := ep.Addresses[0]
			if ep.TargetRef != nil && ep.TargetRef.Name != "" {
				name = ep.TargetRef.Name
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, Member{Name: name, Addr: ep.Addresses[0] + ":" + strconv.Itoa(port)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	m.mu.Lock()
	m.regional, m.regAt = out, time.Now()
	m.mu.Unlock()
	return out, nil
}

// Names lists member names.
func Names(ms []Member) []string {
	out := make([]string, len(ms))
	for i, m := range ms {
		out[i] = m.Name
	}
	return out
}

// Locality reads a node's locality label.
func Locality(ctx context.Context, c client.Reader, node, key string) (string, error) {
	var n corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node}, &n); err != nil {
		return "", err
	}
	v := n.Labels[key]
	if v == "" {
		return "", fmt.Errorf("node %s has no %s label", node, key)
	}
	return v, nil
}

func internalIP(n *corev1.Node) string {
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			return a.Address
		}
	}
	return ""
}
