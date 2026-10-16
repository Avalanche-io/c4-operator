package main

import (
	"fmt"
	"math"
)

// MLState models a wrapped training run (a JobSet of ranks). The queue owns
// when it runs; the training loop checkpoints every CkptEvery steps and
// yields at a checkpoint boundary when asked.
type MLState struct {
	Ranks       int
	TotalSteps  int
	StepSec     float64
	CkptEvery   int
	RestoreSec  float64
	SegStart    float64
	SegStep     float64
	Running     bool
	LastCkpt    int
	Checkpoints []Ckpt
	Yields      []*Yield
	Useful      float64
	Lost        float64
	DatasetView string
	CodeView    string
	BaseView    string
	Wrapped     string

	resumePending bool
	yieldReq      bool
	yieldReason   string
	yieldedAt     float64
	lastResume    float64
	resumeReason  string
}

type Ckpt struct {
	View       string
	Step       int
	At         float64
	BytesNew   float64
	BytesDedup float64
}

type Yield struct {
	At        float64
	Reason    string
	ResumedAt float64
	Resumed   bool
}

func (m *MLState) step(t float64) float64 {
	if !m.Running {
		return float64(m.LastCkpt)
	}
	return math.Min(float64(m.TotalSteps), m.SegStep+math.Max(0, t-m.SegStart-m.RestoreSec)/m.StepSec)
}

// conflict returns a job that shares a pool with j and will miss its
// deadline at current capacity.
func (s *Sim) mlConflict(j *Job) *Job { return s.mlConflictMargin(j, 0) }

// mlConflictMargin also counts jobs with less than margin seconds of slack,
// so a run does not resume into slots a job at risk is about to need.
func (s *Sim) mlConflictMargin(j *Job, margin float64) *Job {
	for _, x := range s.jobs {
		if x == j || x.ML != nil || !x.ready || x.Done || x.Hold {
			continue
		}
		share := false
		for _, p := range x.Pools {
			share = share || contains(j.Pools, p)
		}
		if share && (!x.est.Feasible || x.est.Slack < margin) && x.est.EtaP90 > 0 {
			return x
		}
	}
	return nil
}

func (s *Sim) mlTick() {
	for _, j := range s.jobs {
		m := j.ML
		if m == nil || j.Done {
			continue
		}
		if !m.Running {
			if m.resumePending || j.Hold {
				continue
			}
			c := s.mlConflictMargin(j, 1800)
			switch {
			case c == nil:
				m.resumePending, m.resumeReason = true, "gpu pool has room again"
			case s.t-m.yieldedAt >= 1800:
				m.resumePending, m.resumeReason = true, "starvation guard: 30 min without a running unit"
			}
			continue
		}
		for _, u := range j.Units {
			s.account(j, u, dt)
		}
		cur := m.step(s.t)
		for m.LastCkpt+m.CkptEvery <= int(cur) && m.LastCkpt+m.CkptEvery <= m.TotalSteps {
			m.LastCkpt += m.CkptEvery
			at := m.SegStart + m.RestoreSec + (float64(m.LastCkpt)-m.SegStep)*m.StepSec
			m.Checkpoints = append(m.Checkpoints, Ckpt{
				View: fakeID("ckpt", j.key(), fmt.Sprint(m.LastCkpt)), Step: m.LastCkpt, At: at,
				BytesNew: 6.1e8 + float64(m.LastCkpt%7)*1.3e7, BytesDedup: 1.39e10,
			})
			m.Useful += float64(m.CkptEvery) * m.StepSec * float64(m.Ranks) / 3600
			if m.yieldReq {
				s.mlYield(j, at)
				break
			}
		}
		if !m.Running {
			continue
		}
		if cur >= float64(m.TotalSteps) {
			for _, u := range j.Units {
				a := u.Attempts[len(u.Attempts)-1]
				a.End, a.Ended = s.t, true
				s.release(j, u)
				u.State, u.Node = stSucceeded, ""
			}
			j.Done, j.DoneAt = true, s.t
			continue
		}
		if !m.yieldReq && s.t-m.lastResume > 1800 {
			if c := s.mlConflict(j); c != nil {
				m.yieldReq = true
				m.yieldReason = fmt.Sprintf("%s misses %s at current capacity; gpu is the shortage", c.Name, s.clock(c.eff))
			}
		}
	}
}

func (s *Sim) mlYield(j *Job, at float64) {
	m := j.ML
	m.Running, m.yieldReq = false, false
	m.SegStep = float64(m.LastCkpt)
	m.yieldedAt = at
	m.Yields = append(m.Yields, &Yield{At: at, Reason: m.yieldReason})
	for _, u := range j.Units {
		a := u.Attempts[len(u.Attempts)-1]
		a.End, a.Ended = at, true
		a.Reason = fmt.Sprintf("Yielded at checkpoint step %d", m.LastCkpt)
		s.release(j, u)
		u.Node = ""
		u.State = stQueued
	}
}

// mlTryStart places all ranks at once, or none.
func (s *Sim) mlTryStart(j *Job) bool {
	m := j.ML
	free := 0
	var nodes []*Node
	for _, n := range s.nodes {
		if !contains(j.Pools, n.Pool) || !n.ready(s.t) || contains(j.Blacklist, n.Name) {
			continue
		}
		if f := n.Slots - n.used(); f > 0 {
			free += f
			nodes = append(nodes, n)
		}
	}
	if free < m.Ranks {
		return false
	}
	k := 0
	for _, n := range nodes {
		for f := n.Slots - n.used(); f > 0 && k < m.Ranks; f-- {
			u := j.Units[k]
			u.Node, u.StartAt, u.State = n.Name, s.t, stRunning
			u.Attempts = append(u.Attempts, &Attempt{N: len(u.Attempts) + 1, Node: n.Name, Start: s.t})
			n.running = append(n.running, &runRef{j: j, u: u})
			k++
		}
	}
	m.Running, m.resumePending = true, false
	m.SegStart, m.SegStep = s.t, float64(m.LastCkpt)
	m.lastResume = s.t
	m.Lost += m.RestoreSec * float64(m.Ranks) / 3600
	if len(m.Yields) > 0 {
		y := m.Yields[len(m.Yields)-1]
		y.Resumed, y.ResumedAt = true, s.t
		y.Reason += "; resumed: " + m.resumeReason
	}
	return true
}

func (s *Sim) mlEstimate(j *Job) {
	m := j.ML
	e := &j.est
	left := float64(m.TotalSteps) - m.step(s.t)
	if !m.Running {
		left = float64(m.TotalSteps - m.LastCkpt)
	}
	e.W = left * m.StepSec * float64(m.Ranks)
	e.Sigma = e.W * 0.03
	e.W90 = e.W + 1.28*e.Sigma
	e.TPath = left * m.StepSec
	if !m.Running {
		e.TPath += m.RestoreSec
	}
	e.Attainable = float64(m.Ranks)
	e.Running = 0
	if m.Running {
		e.Running = m.Ranks
	}
	frac := m.step(s.t) / float64(m.TotalSteps)
	for _, u := range j.Units {
		u.Est = float64(m.TotalSteps) * m.StepSec
		u.EstErr = u.Est * 0.03
		_ = frac
	}
}
