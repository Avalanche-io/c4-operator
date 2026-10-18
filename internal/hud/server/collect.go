package server

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	c4v1 "github.com/Avalanche-io/c4-operator/api/v1alpha1"
	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/work/class"
)

// model is the farm as of one collection: everything a snapshot needs,
// unfiltered. Snapshots filter it per viewer.
type model struct {
	at        time.Time
	jobs      []*jobRec
	byKey     map[string]*jobRec
	pools     []c4v1.Pool
	projects  []c4v1.Project
	nsProject map[string]string
	nodes     []corev1.Node
	pods      []corev1.Pod // unit pods
	// podsByJob holds the unfinished unit pods by job key; nil in models
	// built by hand, which scan pods instead.
	podsByJob map[string][]*corev1.Pod
	crs       []c4v1.CapacityRequest
	metrics   farmMetrics
	usage     usageSnapshot
	unitsHour int
	failsHour int
}

type jobRec struct {
	job     c4v1.WorkJob
	key     string
	project string
	plan    *class.Plan
	planErr error
	driver  class.Driver
	// work is JobSummary.work, computed once per collection.
	work    api.Work
	hasWork bool
}

// collect reads the farm. Metrics are scraped every fifth call.
func (s *Server) collect(ctx context.Context) error {
	m := &model{at: time.Now().UTC(), byKey: map[string]*jobRec{}, nsProject: map[string]string{}}
	var (
		jobs     c4v1.WorkJobList
		pools    c4v1.PoolList
		projects c4v1.ProjectList
		nodes    corev1.NodeList
		pods     corev1.PodList
		crs      c4v1.CapacityRequestList
	)
	for _, l := range []struct {
		list client.ObjectList
		opts []client.ListOption
	}{{&jobs, nil}, {&pools, nil}, {&projects, nil}, {&nodes, nil}, {&crs, nil},
		{&pods, []client.ListOption{client.HasLabels{c4v1.LabelJob}}}} {
		if err := s.Client.List(ctx, l.list, l.opts...); err != nil {
			return err
		}
	}
	m.pools, m.projects, m.nodes, m.pods, m.crs = pools.Items, projects.Items, nodes.Items, pods.Items, crs.Items
	sort.Slice(m.pools, func(i, j int) bool { return m.pools[i].Name < m.pools[j].Name })
	sort.Slice(m.projects, func(i, j int) bool { return m.projects[i].Name < m.projects[j].Name })
	sort.Slice(m.nodes, func(i, j int) bool { return m.nodes[i].Name < m.nodes[j].Name })
	for _, p := range m.projects {
		for _, ns := range p.Spec.Namespaces {
			m.nsProject[ns] = p.Name
		}
	}
	prev := s.current()
	for i := range jobs.Items {
		j := jobs.Items[i]
		rec := &jobRec{job: j, key: j.Namespace + "/" + j.Name, project: m.nsProject[j.Namespace]}
		if rec.project == "" {
			rec.project = "namespace/" + j.Namespace
		}
		if d, err := class.Lookup(j.Spec.Class); err == nil {
			rec.driver = d
			if old := prevPlan(prev, rec.key, j.Generation); old != nil {
				rec.plan = old
			} else {
				rec.plan, rec.planErr = d.Plan(&j)
			}
		} else {
			rec.planErr = err
		}
		m.jobs = append(m.jobs, rec)
		m.byKey[rec.key] = rec
	}
	// Dispatch order: ranked jobs first (rank 1 is next), then waiting
	// ones, then finished ones; by creation within each.
	sort.SliceStable(m.jobs, func(a, b int) bool {
		ra, rb := orderKey(&m.jobs[a].job), orderKey(&m.jobs[b].job)
		if ra != rb {
			return ra < rb
		}
		return m.jobs[a].job.CreationTimestamp.Before(&m.jobs[b].job.CreationTimestamp)
	})
	m.indexPods()
	for _, rec := range m.jobs {
		rec.work, rec.hasWork = jobWork(m, rec), true
	}
	s.steady(m)
	if prev == nil || m.at.Sub(prev.metrics.at) >= 5*time.Second {
		m.metrics = s.scrape(ctx)
		if s.Progress != nil {
			s.Progress.Set(m.metrics.at, progressReadings(m.metrics))
		}
	} else {
		m.metrics = prev.metrics
	}
	s.mu.Lock()
	m.usage = s.meter.add(m)
	m.unitsHour, m.failsHour = s.hist.add(m)
	s.m = m
	s.version++
	s.mu.Unlock()
	return nil
}

func prevPlan(prev *model, key string, gen int64) *class.Plan {
	if prev == nil {
		return nil
	}
	if r := prev.byKey[key]; r != nil && r.job.Generation == gen && r.planErr == nil {
		return r.plan
	}
	return nil
}

func orderKey(j *c4v1.WorkJob) int64 {
	switch {
	case j.Status.Phase == "Succeeded" || j.Status.Phase == "Failed":
		return 1 << 40
	case j.Status.Rank > 0:
		return int64(j.Status.Rank)
	}
	return 1 << 30
}

// farmMetrics is what the node daemons and regional replicas report.
type farmMetrics struct {
	at       time.Time
	nodes    map[string]series // by node name
	regional []series
	// pub holds publish latency histogram buckets summed over nodes.
	pub, capture map[float64]float64
}

// series is one process's metrics: sums by name and label subset.
type series []sample

type sample struct {
	name   string
	labels map[string]string
	value  float64
}

// first is the value of the first sample of name whose labels include all
// of want, and whether there is one.
func (ss series) first(name string, want ...string) (float64, bool) {
	for _, s := range ss {
		if s.name != name {
			continue
		}
		ok := true
		for i := 0; i+1 < len(want); i += 2 {
			if s.labels[want[i]] != want[i+1] {
				ok = false
				break
			}
		}
		if ok {
			return s.value, true
		}
	}
	return 0, false
}

// sum adds up the samples of name whose labels include all of want.
func (ss series) sum(name string, want ...string) float64 {
	t := 0.0
	for _, s := range ss {
		if s.name != name {
			continue
		}
		ok := true
		for i := 0; i+1 < len(want); i += 2 {
			if s.labels[want[i]] != want[i+1] {
				ok = false
				break
			}
		}
		if ok {
			t += s.value
		}
	}
	return t
}

// scrape reads /metrics from every node daemon and regional replica.
func (s *Server) scrape(ctx context.Context) farmMetrics {
	fm := farmMetrics{at: time.Now(), nodes: map[string]series{}, pub: map[float64]float64{}, capture: map[float64]float64{}}
	if s.Kube == nil {
		return fm
	}
	pods, err := s.Kube.CoreV1().Pods(s.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "app.kubernetes.io/component in (node,regional)"})
	if err != nil {
		s.Log.Error(err, "list tier pods")
		return fm
	}
	hc := &http.Client{Timeout: 2 * time.Second}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			continue
		}
		comp := p.Labels["app.kubernetes.io/component"]
		port := "7443"
		if comp == "node" {
			port = "7444"
		}
		ss, err := fetchMetrics(ctx, hc, fmt.Sprintf("http://%s:%s/metrics", p.Status.PodIP, port))
		if err != nil {
			continue
		}
		if comp == "node" {
			fm.nodes[p.Spec.NodeName] = ss
			for _, x := range ss {
				switch x.name {
				case "c4_node_publish_seconds_bucket":
					le, _ := strconv.ParseFloat(x.labels["le"], 64)
					fm.pub[le] += x.value
				case "c4_node_capture_seconds_bucket":
					le, _ := strconv.ParseFloat(x.labels["le"], 64)
					fm.capture[le] += x.value
				}
			}
			continue
		}
		fm.regional = append(fm.regional, ss)
	}
	return fm
}

func fetchMetrics(ctx context.Context, hc *http.Client, url string) (series, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseMetrics(resp.Body)
}

// parseMetrics reads the Prometheus text format's samples. Only names
// beginning c4_ are kept.
func parseMetrics(r io.Reader) (series, error) {
	var out series
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "c4_") {
			continue
		}
		sp := strings.LastIndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		v, err := strconv.ParseFloat(line[sp+1:], 64)
		if err != nil {
			continue
		}
		head := line[:sp]
		smp := sample{value: v, labels: map[string]string{}}
		name, rest, hasLabels := strings.Cut(head, "{")
		smp.name = name
		if hasLabels {
			for _, kv := range splitLabels(strings.TrimSuffix(rest, "}")) {
				k, val, _ := strings.Cut(kv, "=")
				smp.labels[k] = strings.Trim(val, `"`)
			}
		}
		out = append(out, smp)
	}
	return out, sc.Err()
}

// splitLabels splits a=b,c="d,e" at commas outside quotes.
func splitLabels(s string) []string {
	var out []string
	inQ, start := false, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			if i == 0 || s[i-1] != '\\' {
				inQ = !inQ
			}
		case ',':
			if !inQ {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

// quantile reads q from cumulative histogram buckets.
func quantile(b map[float64]float64, q float64) float64 {
	var les []float64
	for le := range b {
		les = append(les, le)
	}
	sort.Float64s(les)
	if len(les) == 0 {
		return 0
	}
	total := b[les[len(les)-1]]
	if total == 0 {
		return 0
	}
	target, prev, prevN := q*total, 0.0, 0.0
	for _, le := range les {
		n := b[le]
		if n >= target {
			if math.IsInf(le, 1) {
				return prev
			}
			if n == prevN {
				return le
			}
			return prev + (le-prev)*(target-prevN)/(n-prevN)
		}
		prev, prevN = le, n
	}
	return prev
}

// meter is metered use today: container-hours per project and pool (pod
// run time, not weighted by slots) and GPU-hours (slot time in GPU pools).
// It is what billing is based on (DESIGN.md section 12). It resets at
// midnight in the farm's time zone and lives in memory: a production
// meter writes each interval to the signed ledger instead.
type meter struct {
	day       int
	last      time.Time
	since     time.Time
	container map[[2]string]float64
	gpu       map[string]float64
}

type usageSnapshot struct {
	clusterHours float64
	container    map[[2]string]float64
	gpu          map[string]float64
}

func (mt *meter) add(m *model) usageSnapshot {
	if d := m.at.YearDay(); d != mt.day || mt.container == nil {
		y, mo, dd := m.at.Date()
		// After midnight the day starts at midnight; on the first sample
		// the meter starts now (it cannot know what ran before it).
		since := time.Date(y, mo, dd, 0, 0, 0, 0, m.at.Location())
		if mt.container == nil {
			since = m.at
		}
		mt.day, mt.since, mt.container, mt.gpu, mt.last = d, since, map[[2]string]float64{}, map[string]float64{}, time.Time{}
	}
	if !mt.last.IsZero() {
		h := m.at.Sub(mt.last).Hours()
		gpuPools := map[string]bool{}
		for _, p := range m.pools {
			if p.Spec.Attributes["class"] == "gpu" {
				gpuPools[p.Name] = true
			}
		}
		for _, p := range m.pods {
			if p.Status.Phase != corev1.PodRunning {
				continue
			}
			proj := m.nsProject[p.Namespace]
			pool := p.Labels[c4v1.LabelPool]
			mt.container[[2]string{proj, pool}] += h
			if gpuPools[pool] {
				slots := 1.0
				if len(p.Spec.Containers) > 0 {
					if q, ok := p.Spec.Containers[0].Resources.Requests["c4.avalanche.io/slots"]; ok {
						slots = float64(q.Value())
					}
				}
				mt.gpu[proj] += h * slots
			}
		}
	}
	mt.last = m.at
	out := usageSnapshot{clusterHours: m.at.Sub(mt.since).Hours(), container: map[[2]string]float64{}, gpu: map[string]float64{}}
	for k, v := range mt.container {
		out.container[k] = v
	}
	for k, v := range mt.gpu {
		out.gpu[k] = v
	}
	return out
}

// history keeps a minute-by-minute record of units finished and failed
// farm-wide, for the last hour's rates.
type history struct {
	last    map[string][2]int32 // job -> succeeded, failed at the last sample
	minutes []histMinute
}

type histMinute struct {
	at          time.Time
	done, fails int
}

func (h *history) add(m *model) (int, int) {
	if h.last == nil {
		h.last = map[string][2]int32{}
		for _, j := range m.jobs {
			h.last[j.key] = [2]int32{j.job.Status.Units.Succeeded, j.job.Status.Units.Failed}
		}
	}
	minute := m.at.Truncate(time.Minute)
	if len(h.minutes) == 0 || !h.minutes[len(h.minutes)-1].at.Equal(minute) {
		h.minutes = append(h.minutes, histMinute{at: minute})
	}
	cur := &h.minutes[len(h.minutes)-1]
	for _, j := range m.jobs {
		st := j.job.Status.Units
		old := h.last[j.key]
		if d := st.Succeeded - old[0]; d > 0 {
			cur.done += int(d)
		}
		if d := st.Failed - old[1]; d > 0 {
			cur.fails += int(d)
		}
		h.last[j.key] = [2]int32{st.Succeeded, st.Failed}
	}
	for len(h.minutes) > 0 && m.at.Sub(h.minutes[0].at) > time.Hour {
		h.minutes = h.minutes[1:]
	}
	done, fails := 0, 0
	for _, x := range h.minutes {
		done += x.done
		fails += x.fails
	}
	return done, fails
}

// progressReadings is every unit's reported progress in a scrape, by
// namespace and pod, for the queue's estimates (queue.ProgressCache).
func progressReadings(fm farmMetrics) map[[2]string]float64 {
	out := map[[2]string]float64{}
	for _, ss := range fm.nodes {
		for _, x := range ss {
			if x.name != "c4_task_progress" {
				continue
			}
			out[[2]string{x.labels["namespace"], x.labels["pod"]}] = x.value
		}
	}
	return out
}
