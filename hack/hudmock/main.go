// Command hudmock serves the c4-operator HUD against a synthetic,
// time-evolving farm that implements the HUD API (internal/hud/API.md).
//
//	go run ./hack/hudmock                 # live, on http://127.0.0.1:8740
//	go run ./hack/hudmock --freeze 50m    # stop the clock 50 simulated minutes in
//
// The scenario is a pure function of the seed and the simulated time, so a
// frozen run returns the same data every time.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type object struct {
	project string
	data    []byte
}

type server struct {
	mu       sync.Mutex
	sim      *Sim
	seed     int64
	frozen   bool
	speed    float64
	start    float64
	loop     float64
	objects  map[string]object
	previews map[string]string
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8740", "listen address")
	seed := flag.Int64("seed", 7, "random seed for the synthetic farm")
	speed := flag.Float64("speed", 20, "simulated seconds per real second")
	start := flag.Duration("start", 50*time.Minute, "simulated time to fast-forward to at startup")
	freeze := flag.Duration("freeze", 0, "fast-forward to this simulated time and stop the clock (for screenshots)")
	loop := flag.Duration("loop", 8*time.Hour, "restart the scenario after this much simulated time; 0 never restarts")
	web := flag.String("web", "", "directory holding the HUD web app (default: internal/hud/web in this repository)")
	report := flag.Bool("report", false, "print the scenario's jobs at the start time and exit")
	flag.Parse()

	sv := &server{seed: *seed, speed: *speed, start: start.Seconds(), loop: loop.Seconds(),
		objects: map[string]object{}, previews: map[string]string{}}
	if *freeze > 0 {
		sv.frozen, sv.start = true, freeze.Seconds()
	}
	sv.reset()
	if *report {
		printReport(sv.sim)
		return
	}
	dir := *web
	if dir == "" {
		dir = defaultWebDir()
	}
	if _, err := os.Stat(filepath.Join(dir, "index.html")); err != nil {
		log.Fatalf("hudmock: no index.html in %s (use --web)", dir)
	}
	go sv.clock()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/snapshot", sv.handleSnapshot)
	mux.HandleFunc("GET /api/v1/events", sv.handleEvents)
	mux.HandleFunc("GET /api/v1/graph", sv.handleGraph)
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}", sv.handleJob)
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}/units/{i}", sv.handleUnit)
	mux.HandleFunc("GET /api/v1/jobs/{ns}/{name}/preview", sv.handlePreview)
	mux.HandleFunc("GET /api/v1/objects/{project}/{id}", sv.handleObject)
	mux.HandleFunc("POST /api/v1/jobs/{ns}/{name}/nudge", sv.handleNudge)
	mux.HandleFunc("POST /api/v1/jobs/{ns}/{name}/requeue", sv.handleRequeue)
	mux.HandleFunc("POST /api/v1/jobs/{ns}/{name}/blacklist", sv.handleBlacklist)
	mux.HandleFunc("POST /api/v1/nodes/{name}/disable", sv.handleDisable)
	mux.HandleFunc("POST /api/v1/capacity/{pool}/approve", sv.handleApprove)
	files := http.FileServer(http.Dir(dir))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	state := "live"
	if sv.frozen {
		state = "frozen"
	}
	log.Printf("hudmock: %s at %s, serving %s on http://%s/", state, sv.sim.at(sv.sim.t).Format("Mon 15:04"), dir, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func defaultWebDir() string {
	_, file, _, ok := runtime.Caller(0)
	if ok {
		d := filepath.Join(filepath.Dir(file), "..", "..", "internal", "hud", "web")
		if _, err := os.Stat(d); err == nil {
			return d
		}
	}
	return filepath.Join("internal", "hud", "web")
}

func (sv *server) reset() {
	s := newSim(sv.seed)
	for s.t < sv.start {
		s.step()
	}
	s.estimateAll()
	sv.sim = s
	sv.previews = map[string]string{}
}

func (sv *server) clock() {
	if sv.frozen {
		return
	}
	acc := 0.0
	tick := time.NewTicker(250 * time.Millisecond)
	for range tick.C {
		sv.mu.Lock()
		acc += sv.speed * 0.25
		for acc >= dt {
			sv.sim.step()
			acc -= dt
		}
		if sv.loop > 0 && sv.sim.t >= sv.loop {
			sv.reset()
		}
		sv.mu.Unlock()
	}
}

func viewerOf(r *http.Request) *viewer {
	if v, ok := viewers[r.URL.Query().Get("as")]; ok {
		return v
	}
	return viewers["wrangler"]
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (sv *server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	writeJSON(w, 200, sv.sim.snapshot(viewerOf(r)))
}

func (sv *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "streaming unsupported")
		return
	}
	v := viewerOf(r)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	var last uint64 = math.MaxUint64
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	idle := 0
	for {
		sv.mu.Lock()
		ver := sv.sim.version
		var body []byte
		if ver != last {
			body, _ = json.Marshal(sv.sim.snapshot(v))
			last = ver
		}
		sv.mu.Unlock()
		switch {
		case body != nil:
			fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", body)
			idle = 0
		case idle >= 15:
			fmt.Fprint(w, ": keepalive\n\n")
			idle = 0
		default:
			idle++
		}
		fl.Flush()
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

// job finds a job the viewer may see. Jobs outside the viewer's projects
// are reported as not found, so their names do not leak.
func (sv *server) job(w http.ResponseWriter, r *http.Request) (*Job, *viewer) {
	v := viewerOf(r)
	j := sv.sim.jobBy[r.PathValue("ns")+"/"+r.PathValue("name")]
	if j == nil || !sv.sim.visible(v, j) {
		fail(w, 404, "no such job")
		return nil, nil
	}
	return j, v
}

func (sv *server) handleJob(w http.ResponseWriter, r *http.Request) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	j, v := sv.job(w, r)
	if j == nil {
		return
	}
	writeJSON(w, 200, sv.sim.jobDetail(j, v))
}

func (sv *server) handleUnit(w http.ResponseWriter, r *http.Request) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	j, _ := sv.job(w, r)
	if j == nil {
		return
	}
	i, err := strconv.Atoi(r.PathValue("i"))
	if err != nil || i < 0 || i >= len(j.Units) || j.Units[i] == nil {
		fail(w, 404, "no such unit")
		return
	}
	writeJSON(w, 200, sv.sim.unitDetail(j, j.Units[i]))
}

func (sv *server) handlePreview(w http.ResponseWriter, r *http.Request) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	j, _ := sv.job(w, r)
	if j == nil {
		return
	}
	if j.Fps == 0 || len(j.Passes) == 0 {
		fail(w, 404, "this job has no preview")
		return
	}
	pass := j.Passes[len(j.Passes)-1]
	var us []*Unit
	for _, u := range j.Units {
		if u != nil && u.Group == pass {
			us = append(us, u)
		}
	}
	sort.Slice(us, func(a, b int) bool { return us[a].Pos < us[b].Pos })
	first, last := j.AxisStart, j.AxisStart+(len(us)-1)*j.AxisStep
	var frames []map[string]any
	for _, u := range us {
		f := j.AxisStart + u.Pos*j.AxisStep
		fm := map[string]any{"frame": f}
		if u.State == stSucceeded {
			key := j.key() + "#" + strconv.Itoa(f)
			id, ok := sv.previews[key]
			if !ok {
				heavy := 0.0
				for _, h := range j.Heavy {
					heavy = math.Max(heavy, (heavyFactor(u.Pos, h)-1)/(h.Factor-1))
				}
				data := renderFrame(j.Name, f, first, last, heavy)
				id = c4id(data)
				sv.objects[id] = object{project: j.Project, data: data}
				sv.previews[key] = id
			}
			fm["url"] = "/api/v1/objects/" + j.Project + "/" + id
		}
		frames = append(frames, fm)
	}
	writeJSON(w, 200, map[string]any{"fps": j.Fps, "frames": frames})
}

func (sv *server) handleObject(w http.ResponseWriter, r *http.Request) {
	sv.mu.Lock()
	o, ok := sv.objects[r.PathValue("id")]
	sv.mu.Unlock()
	p := r.PathValue("project")
	if !ok || o.project != p || !viewerOf(r).sees(p) {
		fail(w, 404, "no such object")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	_, _ = w.Write(o.data)
}

func (sv *server) handleGraph(w http.ResponseWriter, r *http.Request) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	writeJSON(w, 200, sv.sim.graph(viewerOf(r)))
}

// after a control, the estimate and order are recomputed at once so the HUD
// shows the effect on its next update.
func (sv *server) changed() {
	sv.sim.estimateAll()
	sv.sim.version++
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
		fail(w, 400, "body is not valid JSON: "+err.Error())
		return false
	}
	return true
}

func (sv *server) handleNudge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hold     *bool    `json:"hold"`
		HoldStop *bool    `json:"holdStop"`
		Boost    *float64 `json:"boost"`
		Deadline *string  `json:"deadline"`
	}
	if !decode(w, r, &body) {
		return
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	j, _ := sv.job(w, r)
	if j == nil {
		return
	}
	s := sv.sim
	var did []string
	if body.Boost != nil {
		if *body.Boost < 1 || *body.Boost > 2 {
			fail(w, 400, "boost must be between 1 and 2")
			return
		}
		j.Boost = *body.Boost
		did = append(did, fmt.Sprintf("boost set to %.2g", j.Boost))
	}
	if body.Deadline != nil {
		t, err := time.Parse(time.RFC3339, *body.Deadline)
		if err != nil {
			fail(w, 400, "deadline must be RFC 3339, for example 2026-10-23T18:00:00-07:00")
			return
		}
		j.Deadline, j.HasDeadline = t.Sub(s.epoch).Seconds(), true
		did = append(did, "deadline set to "+s.clock(j.Deadline))
	}
	if body.Hold != nil {
		j.Hold = *body.Hold
		if !j.Hold {
			j.HoldStop = false
		}
		did = append(did, map[bool]string{true: "held", false: "released"}[j.Hold])
	}
	if body.HoldStop != nil && *body.HoldStop {
		j.Hold, j.HoldStop = true, true
		for _, u := range j.Units {
			if u == nil || u.Node == "" {
				continue
			}
			a := u.Attempts[len(u.Attempts)-1]
			a.End, a.Ended, a.Reason = s.t, true, "Stopped by hold"
			s.release(j, u)
			u.Node, u.FailAt = "", 0
			if u.State == stRunning {
				u.State = stQueued
				if u.Gen {
					u.State = stUnevaluated
				}
			}
		}
		did = append(did, "held and running units stopped")
	}
	sv.changed()
	writeJSON(w, 200, map[string]any{"ok": true, "message": j.Name + ": " + strings.Join(did, ", "), "job": s.jobSummary(j)})
}

func (sv *server) handleRequeue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Units json.RawMessage `json:"units"`
	}
	if !decode(w, r, &body) {
		return
	}
	var idx []int
	var str string
	if err := json.Unmarshal(body.Units, &str); err == nil {
		idx = parseSet(str)
	} else if err := json.Unmarshal(body.Units, &idx); err != nil {
		fail(w, 400, `units must be a run-length set such as "12,40-44" or an array of indexes`)
		return
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	j, _ := sv.job(w, r)
	if j == nil {
		return
	}
	n := 0
	for _, i := range idx {
		if i < 0 || i >= len(j.Units) || j.Units[i] == nil {
			continue
		}
		u := j.Units[i]
		if u.Node != "" || u.State == stUnevaluated || u.Gen {
			continue
		}
		u.State, u.Fails, u.FailPlan, u.FailAt = stQueued, 0, 0, 0
		n++
	}
	if n > 0 {
		j.FailedJob = false
		j.cursor = 0
	}
	sv.changed()
	writeJSON(w, 200, map[string]any{"ok": true, "message": fmt.Sprintf("%s: %d units requeued", j.Name, n), "job": sv.sim.jobSummary(j)})
}

func (sv *server) handleBlacklist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Node   string `json:"node"`
		Remove bool   `json:"remove"`
	}
	if !decode(w, r, &body) {
		return
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	j, _ := sv.job(w, r)
	if j == nil {
		return
	}
	if sv.sim.nodeBy(body.Node) == nil {
		fail(w, 404, "no such node")
		return
	}
	msg := "blacklisted " + body.Node
	switch {
	case body.Remove:
		out := j.Blacklist[:0]
		for _, n := range j.Blacklist {
			if n != body.Node {
				out = append(out, n)
			}
		}
		j.Blacklist = out
		msg = "removed " + body.Node + " from the blacklist"
	case !contains(j.Blacklist, body.Node):
		j.Blacklist = append(j.Blacklist, body.Node)
	}
	sv.changed()
	writeJSON(w, 200, map[string]any{"ok": true, "message": j.Name + ": " + msg, "job": sv.sim.jobSummary(j)})
}

func (sv *server) handleDisable(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Disabled bool `json:"disabled"`
	}
	if !decode(w, r, &body) {
		return
	}
	sv.mu.Lock()
	defer sv.mu.Unlock()
	n := sv.sim.nodeBy(r.PathValue("name"))
	if n == nil {
		fail(w, 404, "no such node")
		return
	}
	n.Disabled = body.Disabled
	sv.changed()
	verb := map[bool]string{true: "disabled farm-wide; running units finish, no new units start", false: "enabled"}[n.Disabled]
	writeJSON(w, 200, map[string]any{"ok": true, "message": n.Name + " " + verb, "node": sv.sim.nodeSummary(n, viewerOf(r))})
}

func (sv *server) handleApprove(w http.ResponseWriter, r *http.Request) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	v := viewerOf(r)
	pool := r.PathValue("pool")
	for _, c := range sv.sim.capReqs {
		if c.Pool != pool || c.State != "PendingApproval" || !v.sees(c.Project) {
			continue
		}
		sv.sim.approve(c)
		sv.changed()
		writeJSON(w, 200, map[string]any{"ok": true, "message": fmt.Sprintf("Burst approved: %d %s slots, est. $%.0f", c.Slots, c.Pool, c.EstCost)})
		return
	}
	fail(w, 404, "no capacity request awaiting approval in pool "+pool)
}

func printReport(s *Sim) {
	fmt.Printf("now %s\n", s.at(s.t).Format("Mon 15:04:05"))
	for i, j := range s.ranked() {
		if s.t < j.Created {
			continue
		}
		e := j.est
		dl := "-"
		if j.hasEff {
			dl = s.clock(j.eff)
		}
		done, total := 0, 0
		for _, u := range j.Units {
			if u == nil {
				continue
			}
			total++
			if u.State == stSucceeded {
				done++
			}
		}
		fmt.Printf("%2d %-26s %-9s %5d/%-5d run %2d R %5.1f A %4.1f dl %s p50 %s p90 %s slack %6.0fm feas %v\n",
			i+1, j.Name, s.phase(j), done, total, e.Running, e.Required, e.Attainable, dl, s.clock(e.EtaP50), s.clock(e.EtaP90), e.Slack/60, e.Feasible)
		if e.Message != "" {
			fmt.Printf("   %s\n", e.Message)
		}
		if len(j.Expansions) > 0 {
			fmt.Printf("   expanded %d\n", len(j.Expansions))
		}
		if j.ML != nil {
			fmt.Printf("   ml step %.0f running %v yields %d\n", j.ML.step(s.t), j.ML.Running, len(j.ML.Yields))
		}
	}
	for _, c := range s.capReqs {
		fmt.Printf("cap %s %d slots $%.0f %s\n", c.Pool, c.Slots, c.EstCost, c.State)
	}
}
