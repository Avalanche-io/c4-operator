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
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/Avalanche-io/c4"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/farmbook"
	"github.com/Avalanche-io/c4-operator/internal/provenance/people"
	"github.com/Avalanche-io/c4-operator/internal/storeurl"
)

const (
	farmBookDefault = 1000
	farmBookMax     = 5000
)

// keepFarmRecord puts a farm-level act's control record into the system
// store as it is signed, so the queue finds it when its entry is sealed,
// and the person's act it cites beside it, by the C4 ID it cites, so the
// book can show whose device signed (farmbook.Reader.Act). A record kept
// but never booked (its write failed) counts for nothing.
func (s *Server) keepFarmRecord(ctx context.Context, pred controlPredicate, rec []byte) {
	if s.FarmBook == nil || !farmbook.IsControlAct(pred.Action) {
		return
	}
	if pred.Act != nil && pred.actBundle != nil {
		if err := s.FarmBook.Store.Put(ctx, pred.Act.Record, pred.actBundle); err != nil {
			s.Log.Error(err, "keep a control record's act for the farm book", "act", pred.Act.Record)
		}
	}
	id := c4.Identify(bytes.NewReader(rec)).String()
	if err := s.FarmBook.Store.Put(ctx, id, rec); err != nil {
		s.Log.Error(err, "keep a control record for the farm book", "record", id)
	}
}

// bookActs reads the farm book once, when first asked, for the person's
// act each control record cites: by the entry that books the record,
// sealed or pending (farmbook.Reader.Act), or nil. A farm with no farm
// book, or a record it doesn't book, shows none.
func (s *Server) bookActs(ctx context.Context) func(record string) *api.Act {
	var view *farmbook.View
	read := false
	return func(record string) *api.Act {
		if s.FarmBookReader == nil || record == "" {
			return nil
		}
		if !read {
			read = true
			var err error
			if view, err = s.FarmBookReader.View(ctx, time.Now()); err != nil {
				s.Log.Error(err, "read the farm book for the acts it books")
			}
		}
		if view == nil {
			return nil
		}
		if pos := view.Booking(record); pos > 0 {
			return s.FarmBookReader.Act(ctx, view, pos)
		}
		return nil
	}
}

// offerEvery is how often one node's enrolment offers are booked: a host
// runner offering again and again books one in that time, so no runner can
// flood the book (the offers themselves are kept in memory, as before).
const offerEvery = time.Minute

// offersBooked is when each node's last offer was booked.
var offersBooked sync.Map // node -> time.Time

// bookOffer books a node's enrolment offer, at most once each offerEvery.
func (s *Server) bookOffer(ctx context.Context, node, record string) {
	now := time.Now()
	if last, ok := offersBooked.Load(node); ok && now.Sub(last.(time.Time)) < offerEvery {
		s.Log.Info("an enrolment offer not booked: the node offered within the minute", "node", node)
		return
	}
	offersBooked.Store(node, now)
	s.bookFarm(ctx, record)
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
	if err == nil && c4.Identify(bytes.NewReader(rec)).String() != record {
		// the entry books the ID of the bytes, so another record's bytes
		// would be booked as if they were this act's
		err = fmt.Errorf("%s: %w", record, storeurl.ErrNotItsID)
	}
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
// seal. A device entry's addresses are in its private part, which goes
// only to its person and the farm's admins; anyone else gets the entry
// without it, labelled, and it still checks against its ID (PEOPLE-KEYS-2
// 10.4 item 19).
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
			if !isControlEntry(es[k].Entry) {
				private, red, err := people.Private(ctx, s.FarmBookReader.Store, es[k].Entry, sees)
				if err != nil {
					fail(w, http.StatusInternalServerError, "entry %d: %v", es[k].Position, err)
					return
				}
				es[k].Private, es[k].Redacted = private, red
				if slices.Contains(red, farmbook.RedactedEntry) {
					// withheld whole: nothing of it, its act neither
					es[k].Entry = nil
					continue
				}
			}
			es[k].Act = s.FarmBookReader.Act(ctx, view, es[k].Position)
			es[k].Binding = s.FarmBookReader.Signer(ctx, view, es[k].Position)
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
