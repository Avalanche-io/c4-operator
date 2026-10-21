package dcc

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"strings"
	"unicode/utf16"
)

// plistString reads one top-level string value (such as
// CFBundleShortVersionString) from a macOS Info.plist, XML or binary.
// It reads the file and nothing else.
func plistString(file, key string) (string, error) {
	b, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	if bytes.HasPrefix(b, []byte("bplist00")) {
		return bplistString(b, key)
	}
	return xmlPlistString(b, key)
}

// xmlPlistString finds <key>key</key><string>value</string> directly in
// the plist's top <dict>.
func xmlPlistString(b []byte, key string) (string, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	depth, lastKey := 0, ""
	for {
		tok, err := d.Token()
		if err == io.EOF {
			return "", errors.New("no " + key + " in the plist")
		}
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			depth++
			if depth != 3 { // plist > dict > key|string
				continue
			}
			var s string
			if err := d.DecodeElement(&s, &t); err != nil {
				return "", err
			}
			depth--
			switch {
			case t.Name.Local == "key":
				lastKey = s
			case lastKey == key && t.Name.Local == "string":
				return strings.TrimSpace(s), nil
			default:
				lastKey = ""
			}
		case xml.EndElement:
			depth--
		}
	}
}

// bplistString reads a binary plist ("bplist00", Apple's documented
// format) just far enough to find one string in its top dictionary.
func bplistString(b []byte, key string) (string, error) {
	bad := errors.New("not a binary plist this reader understands")
	if len(b) < 8+32 {
		return "", bad
	}
	tr := b[len(b)-32:]
	offSize, refSize := int(tr[6]), int(tr[7])
	numObjs := binary.BigEndian.Uint64(tr[8:16])
	top := binary.BigEndian.Uint64(tr[16:24])
	tableAt := binary.BigEndian.Uint64(tr[24:32])
	if offSize < 1 || offSize > 8 || refSize < 1 || refSize > 8 || numObjs == 0 || top >= numObjs ||
		tableAt >= uint64(len(b)) || numObjs > uint64(len(b)) || tableAt+numObjs*uint64(offSize) > uint64(len(b)) {
		return "", bad
	}
	be := func(p []byte) uint64 {
		var v uint64
		for _, c := range p {
			v = v<<8 | uint64(c)
		}
		return v
	}
	offset := func(obj uint64) (int, bool) {
		if obj >= numObjs {
			return 0, false
		}
		at := tableAt + obj*uint64(offSize)
		o := be(b[at : at+uint64(offSize)])
		return int(o), o < uint64(len(b))
	}
	// length reads an object's count: the marker's low nibble, or an int
	// object after it when the nibble is 0xF. It returns where the
	// object's contents start.
	length := func(at int) (n, start int, ok bool) {
		n = int(b[at] & 0x0F)
		start = at + 1
		if n != 0x0F {
			return n, start, true
		}
		if start >= len(b) || b[start]>>4 != 0x1 {
			return 0, 0, false
		}
		w := 1 << (b[start] & 0x0F)
		if w > 8 || start+1+w > len(b) {
			return 0, 0, false
		}
		v := be(b[start+1 : start+1+w])
		if v > uint64(len(b)) {
			return 0, 0, false
		}
		return int(v), start + 1 + w, true
	}
	str := func(obj uint64) (string, bool) {
		at, ok := offset(obj)
		if !ok {
			return "", false
		}
		n, start, ok := length(at)
		if !ok {
			return "", false
		}
		switch b[at] >> 4 {
		case 0x5: // ASCII
			if start+n > len(b) {
				return "", false
			}
			return string(b[start : start+n]), true
		case 0x6: // UTF-16 big-endian
			if start+2*n > len(b) {
				return "", false
			}
			u := make([]uint16, n)
			for i := range u {
				u[i] = binary.BigEndian.Uint16(b[start+2*i:])
			}
			return string(utf16.Decode(u)), true
		}
		return "", false
	}
	at, ok := offset(top)
	if !ok || b[at]>>4 != 0xD {
		return "", bad
	}
	n, start, ok := length(at)
	if !ok || start+2*n*refSize > len(b) {
		return "", bad
	}
	ref := func(i int) uint64 { return be(b[start+i*refSize : start+(i+1)*refSize]) }
	for i := 0; i < n; i++ {
		if k, ok := str(ref(i)); ok && k == key {
			if v, ok := str(ref(n + i)); ok {
				return strings.TrimSpace(v), nil
			}
			return "", errors.New(key + " in the plist is not a string")
		}
	}
	return "", errors.New("no " + key + " in the plist")
}
