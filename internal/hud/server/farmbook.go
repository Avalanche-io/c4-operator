package server

// The farm book (internal/farmbook, internal/hud/API.md, GET /book/farm):
// the farm-wide, hash-linked log of farm-level acts. The HUD writes an
// entry for each farm-level act it signs as a control record (the acts
// farmbook.ControlActs names) once the write that carries it out has
// succeeded, so the book holds acts that took effect; the queue checks
// each against its record and seals it. Any replica serves the book, and
// reads it through a reader that keeps the last head it saw, so this HUD
// too refuses a head that goes back.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
)

const (
	farmBookDefault = 1000
	farmBookMax     = 5000
)

// keepFarmRecord puts a farm-level act's control record into the system
// store as it is signed, so the queue finds it when its entry is sealed.
// A record kept but never booked (its write failed) counts for nothing.
func (s *Server) keepFarmRecord(ctx context.Context, action string, rec []byte) {
	if s.FarmBook == nil || !farmbook.IsControlAct(action) {
		return
	}
	id := c4.Identify(bytes.NewReader(rec)).String()
	if err := s.FarmBook.Store.Put(ctx, id, rec); err != nil {
		s.Log.Error(err, "keep a control record for the farm book", "record", id)
	}
}

// bookFarm writes the entry for a farm-level act's control record, kept
// by keepFarmRecord, for the seal. A failure is logged: the act took
// effect, and an act missing from the book is slice 2's violation to
// show (S2c), never a refusal here.
func (s *Server) bookFarm(ctx context.Context, record string) {
	if s.FarmBook == nil || record == "" {
		return
	}
	rec, err := s.FarmBook.Store.Get(ctx, record)
	if err != nil {
		s.Log.Error(err, "book a control record in the farm book", "record", record)
		return
	}
	e, err := farmbook.ControlEntry(rec, time.Now())
	if err != nil {
		s.Log.Error(err, "book a control record in the farm book", "record", record)
		return
	}
	if _, err := s.FarmBook.Append(ctx, farmbook.Submission{Entry: e}); err != nil {
		s.Log.Error(err, "book a control record in the farm book", "record", record)
	}
}

// handleFarmBook is GET /book/farm?since=<position>&limit=: the signed
// segments after since, the entries they seal and the entries pending the
// seal. Device entries' addresses go only to their person and the farm's
// admins; anyone else gets them redacted, labelled (PEOPLE-KEYS-2 10.4
// item 19).
func (s *Server) handleFarmBook(w http.ResponseWriter, r *http.Request) {
	v := s.withViewer(w, r)
	if v == nil {
		return
	}
	if v.Unit != nil {
		fail(w, http.StatusForbidden, "%s is a unit: the farm book is read by people", v.User)
		return
	}
	if s.FarmBookReader == nil {
		fail(w, http.StatusNotFound, "this farm keeps no farm book: its install names no system store (--system-store)")
		return
	}
	since, limit := int64(0), farmBookDefault
	if q := r.URL.Query().Get("since"); q != "" {
		n, err := strconv.ParseInt(q, 10, 64)
		if err != nil || n < 0 {
			fail(w, http.StatusBadRequest, "since must be a book position (0 for the start)")
			return
		}
		since = n
	}
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 1 {
			fail(w, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, farmBookMax)
	}
	ctx := r.Context()
	view, err := s.FarmBookReader.View(ctx, time.Now())
	if err != nil {
		code := http.StatusServiceUnavailable
		if errors.Is(err, farmbook.ErrRollback) || errors.Is(err, farmbook.ErrBook) {
			// Not a moment's trouble: the book this HUD read doesn't hold
			// together, or went back. Say so plainly.
			code = http.StatusConflict
		}
		fail(w, code, "the farm book: %v", err)
		return
	}
	page := farmbook.NewPage(view, since, limit)
	caller := s.peopleCaller(r)
	iss, _ := s.People.(*people.Issuer)
	sees := func(person string) bool {
		if iss != nil {
			return iss.Sees(ctx, caller, person)
		}
		return caller != "" && caller == person
	}
	for _, es := range [][]farmbook.PageEntry{page.Entries, page.Pending} {
		for k := range es {
			if isControlEntry(es[k].Entry) {
				continue
			}
			raw, red, err := people.RedactEntry(es[k].Entry, sees)
			if err != nil {
				fail(w, http.StatusInternalServerError, "entry %d: %v", es[k].Position, err)
				return
			}
			es[k].Entry, es[k].Redacted = raw, red
		}
	}
	writeJSON(w, http.StatusOK, page)
}

// isControlEntry reports whether an entry books a control record, which
// names no addresses.
func isControlEntry(raw []byte) bool {
	var e struct {
		Kind string `json:"kind"`
	}
	json.Unmarshal(raw, &e)
	return e.Kind == farmbook.KindControl
}
