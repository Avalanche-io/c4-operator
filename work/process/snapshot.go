package process

// BookSnapshot is a Book as JSON: what the queue persists, per project,
// so the record (and the verification it has earned) survives a restart.
// The format is experimental: it will change with C4's derivation-record
// design.
type BookSnapshot struct {
	Entries []snapEntry            `json:"entries"`
	Procs   map[string]*procRecord `json:"procs"`
}

type snapEntry struct {
	Key   Key    `json:"key"`
	Entry *entry `json:"entry"`
}

// Snapshot returns the book's contents and clears its changed flag.
func (b *Book) Snapshot() BookSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dirty = false
	s := BookSnapshot{Procs: map[string]*procRecord{}}
	for k, e := range b.entries {
		cp := *e
		cp.Outputs = map[string]int{}
		for o, n := range e.Outputs {
			cp.Outputs[o] = n
		}
		if e.Changesets != nil {
			cp.Changesets, cp.Records = map[string]string{}, map[string]string{}
			for o, c := range e.Changesets {
				cp.Changesets[o], cp.Records[o] = c, e.Records[o]
			}
		}
		s.Entries = append(s.Entries, snapEntry{Key: k, Entry: &cp})
	}
	for k, p := range b.procs {
		cp := *p
		s.Procs[k] = &cp
	}
	return s
}

// Changed reports whether the book changed since the last Snapshot.
func (b *Book) Changed() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.dirty
}

// RestoreBook returns a book with a snapshot's contents.
func RestoreBook(s BookSnapshot) *Book {
	b := &Book{}
	b.init()
	for _, se := range s.Entries {
		if se.Entry == nil {
			continue
		}
		e := *se.Entry
		if e.Outputs == nil {
			e.Outputs = map[string]int{}
		}
		b.entries[se.Key] = &e
		for out := range e.Outputs {
			b.byOutput[out] = se.Key
		}
	}
	for k, p := range s.Procs {
		if p != nil {
			cp := *p
			b.procs[k] = &cp
		}
	}
	return b
}
