package telemetry

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
)

var checked sync.Map // reflect.Type → error

// Encode is the one encoder every telemetry output is written with. It
// refuses a record whose type could hold the work (CheckType) before it
// writes a byte, so a field added in the wrong shape fails here as well as
// in the structural test.
func Encode(r Record) ([]byte, error) {
	t := reflect.TypeOf(r)
	if err := CheckType(t); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}

// CheckType says whether t can hold only telemetry's closed kinds: no
// string, byte slice, interface, error, map, function or channel anywhere
// in it, no named type from outside this package, pointers only to this
// package's structs, and an enum table, beginning with "other", behind
// every enum.
func CheckType(t reflect.Type) error {
	if v, ok := checked.Load(t); ok {
		if v == nil {
			return nil
		}
		return v.(error)
	}
	err := checkType(t, nil)
	if err == nil {
		checked.Store(t, nil)
	} else {
		checked.Store(t, err)
	}
	return err
}

var (
	pkgPath  = reflect.TypeOf(Int(0)).PkgPath()
	enumType = reflect.TypeOf((*Enum)(nil)).Elem()
)

func checkType(t reflect.Type, path []string) error {
	where := func() string {
		if len(path) == 0 {
			return t.String()
		}
		return fmt.Sprint(path)
	}
	if t.Name() != "" && t.PkgPath() != "" && t.PkgPath() != pkgPath {
		return fmt.Errorf("%s: %s is from outside telemetry", where(), t)
	}
	if t.Implements(enumType) {
		if t.Kind() != reflect.Uint16 {
			return fmt.Errorf("%s: enum %s is not a uint16", where(), t)
		}
		table := reflect.Zero(t).Interface().(Enum).Table()
		if len(table) == 0 || table[0] != "other" {
			return fmt.Errorf("%s: enum %s has no table beginning with other", where(), t)
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float64:
		if t.Kind() == reflect.Uint16 && t.Name() != "" && t.PkgPath() == pkgPath && !t.Implements(enumType) {
			return fmt.Errorf("%s: %s is a uint16 with no enum table", where(), t)
		}
		return nil
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if err := checkType(f.Type, append(path, t.Name()+"."+f.Name)); err != nil {
				return err
			}
		}
		return nil
	case reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			return fmt.Errorf("%s: a byte slice can hold text", where())
		}
		return checkType(t.Elem(), path)
	case reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			if t.Len() <= 32 {
				return nil
			}
			return fmt.Errorf("%s: a %d-byte array can hold text", where(), t.Len())
		}
		return checkType(t.Elem(), path)
	case reflect.Pointer:
		if t.Elem().Kind() != reflect.Struct || t.Elem().PkgPath() != pkgPath {
			return fmt.Errorf("%s: a pointer to %s", where(), t.Elem())
		}
		return checkType(t.Elem(), path)
	}
	return fmt.Errorf("%s: %s (%s) can hold the work", where(), t, t.Kind())
}
