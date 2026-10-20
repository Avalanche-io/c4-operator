package class

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Documents units write (expansion, verdict, usage) and plan files are
// read strictly: a field the format does not have is an error naming it,
// so an old or misspelled document fails loudly instead of being misread
// (a unit whose does was ignored would run as a procedure).

// Renamed are fields the formats have renamed, with where they went; an
// unknown field of one of these names says so.
var Renamed = map[string]string{
	"does": "reasoning.does",
	"kind": "reasoning.does (model or procedure)",
}

// Strict decodes JSON doc into v, refusing unknown fields.
func Strict(doc []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return explain(err)
	}
	return nil
}

// explain names a renamed field's new place in an unknown-field error.
func explain(err error) error {
	f, ok := strings.CutPrefix(err.Error(), "json: unknown field ")
	if !ok {
		return err
	}
	name, uerr := strconv.Unquote(f)
	if uerr != nil {
		return err
	}
	if to, ok := Renamed[name]; ok {
		return fmt.Errorf("unknown field %q: it is now %s", name, to)
	}
	return fmt.Errorf("unknown field %q", name)
}
