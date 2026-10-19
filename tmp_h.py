import sys

root = sys.argv[1]


def edit(path, pairs):
    p = root + "/" + path
    s = open(p).read()
    for old, new in pairs:
        assert s.count(old) == 1, (path, s.count(old), old[:70])
        s = s.replace(old, new, 1)
    open(p, "w").write(s)


edit("internal/work/queue/queue.go", [
    ('''	v.runnable, v.waiting, v.stuck = unitWaits(plan, m, v)
	q.serveWaits(v, now)
''', '''	v.runnable, v.waiting, v.stuck = unitWaits(plan, m, v)
	q.serveWaits(v, now)
	applyHolds(v)
'''),
    ('''	openWaits map[int]bool
	planWait  string
}''', '''	openWaits map[int]bool
	planWait  string
	// locked are units waiting for a holder of what they claim (holds.go).
	locked map[int]lockInfo
}'''),
    ('''	for _, idx := range v.runnable {
		queued = append(queued, unit(idx))
	}
''', '''	for _, idx := range v.runnable {
		queued = append(queued, unit(idx))
	}
	for idx := range v.locked {
		queued = append(queued, unit(idx))
	}
'''),
    ('''	for _, idx := range v.runnable {
		u := plan.Units[idx]
		rem[idx] = e.Predict(u.Key, u.Pos).Mean
	}
''', '''	for _, idx := range v.runnable {
		u := plan.Units[idx]
		rem[idx] = e.Predict(u.Key, u.Pos).Mean
	}
	for idx := range v.locked {
		u := plan.Units[idx]
		rem[idx] = e.Predict(u.Key, u.Pos).Mean
	}
'''),
    ('''	s.TPath = criticalPath(plan, v.mem, rem, func(key string) float64 { return e.Generator(key).Longest })''',
     '''	s.TPath = criticalPath(plan, v.mem, rem, func(key string) float64 { return e.Generator(key).Longest })
	if len(rem) <= 2000 {
		// Units that must take turns for what they claim run one after
		// another.
		s.TPath = math.Max(s.TPath, holdChains(plan, rem))
	}'''),
    ('''	j.Blocked = len(v.waiting) - len(v.stuck)''', '''	j.Blocked = len(v.waiting) - len(v.stuck) + len(v.locked)'''),
    ('''	for _, d := range dec.Dispatch {
		key, idx, ok := parseUnitID(d.Unit)
		v := byKey[key]
		p := pools[d.Pool]
		if !ok || v == nil || p == nil {
			continue
		}
''', '''	guard := claimGuard{}
	for _, d := range dec.Dispatch {
		key, idx, ok := parseUnitID(d.Unit)
		v := byKey[key]
		p := pools[d.Pool]
		if !ok || v == nil || p == nil {
			continue
		}
		if !guard.admit(v, idx) {
			// Its claims conflict with a unit started this tick: it waits.
			continue
		}
'''),
])
edit("internal/work/queue/report.go", [
    ('''		case isWaiting[i]:''', '''		case hasLock(v, i):
			lockedU = append(lockedU, i)
		case isWaiting[i]:'''),
    ('''	var succ, egress, fail, flag, accepted,''', '''	var lockedU []int
	var succ, egress, fail, flag, accepted,'''),
    ('''Flagged: int32(len(flag))''', '''Flagged: int32(len(flag)), Locked: int32(len(lockedU))'''),
    ('''"flagged": flag} {''', '''"flagged": flag, "locked": lockedU} {'''),
    ('''	st.Expansions = expansionStatus(m.expansions)''', '''	st.Expansions = expansionStatus(m.expansions)
	st.Holds = holdsStatus(v)'''),
])
with open(root + "/internal/work/queue/holds.go", "a") as f:
    f.write('''
func hasLock(v *view, i int) bool {
	_, ok := v.locked[i]
	return ok
}
''')
