package schema

import (
	_ "embed"
	"slices"
)

// The farm book's sealed segment (book/v1): one run of the farm-wide,
// hash-linked log of farm-level acts, signed by the queue that sealed it
// (component/queue). It is an in-toto statement in a DSSE envelope, as
// every C4 record is.
const BookPredicateType = "https://cccc.io/provenance/book/v1"

//go:embed book-1.schema.json
var bookSchema []byte

// BookSchema is the JSON Schema of a farm book segment (book/v1), as GET
// /schema/book serves it.
func BookSchema() []byte { return slices.Clone(bookSchema) }

// BookSegment is one sealed segment as the queue signed it: the statement
// in a DSSE payload.
type BookSegment struct {
	Type          string               `json:"_type"`
	Subject       []Subject            `json:"subject"`
	PredicateType string               `json:"predicateType"`
	Predicate     BookSegmentPredicate `json:"predicate"`
}

// BookSegmentPredicate is what a segment says.
type BookSegmentPredicate struct {
	Farm string `json:"farm"`
	// Number counts from 1; Prev is the C4 ID of the segment before, as
	// signed, and the first has none.
	Number int64  `json:"number"`
	Prev   string `json:"prev,omitempty"`
	// First and Last are the positions it seals; Last is First-1 when it
	// seals none and only refuses. Entries are the C4 IDs of the entries
	// at First to Last.
	First   int64    `json:"first"`
	Last    int64    `json:"last"`
	Entries []string `json:"entries"`
	// Refused are entries written for the seal that it refused; Flags are
	// entries it sealed whose record's signedAt falls outside the window
	// their position allows.
	Refused []BookRefusal `json:"refused,omitempty"`
	Flags   []BookFlag    `json:"flags,omitempty"`
	// Adopted is set on the segment that seals a people's index kept
	// before the book, unchanged, as the book's first entries.
	Adopted *BookAdopted `json:"adopted,omitempty"`
	// SignedAt is when the queue sealed it: the time every position it
	// seals is judged at.
	SignedAt string `json:"signedAt"`
}

// BookRefusal is an entry the seal refused: never sealed, never counted.
type BookRefusal struct {
	Submission string `json:"submission"`
	Kind       string `json:"kind,omitempty"`
	Record     string `json:"record,omitempty"`
	Why        string `json:"why"`
}

// BookFlag is a sealed entry whose record says it was signed outside the
// window its position allows: after From and before To.
type BookFlag struct {
	Position int64  `json:"position"`
	Record   string `json:"record"`
	SignedAt string `json:"signedAt"`
	From     string `json:"from"`
	To       string `json:"to"`
}

// BookAdopted names the people's index a book's first segment adopted.
type BookAdopted struct {
	Head     string `json:"head"`
	Position int64  `json:"position"`
	Order    string `json:"order"`
}

// ParseBookSegment reads a segment statement, checking its shape.
func ParseBookSegment(b []byte) (*BookSegment, []Error) {
	var s BookSegment
	if errs := parse(b, "book", &s); errs != nil {
		return nil, errs
	}
	return &s, nil
}
