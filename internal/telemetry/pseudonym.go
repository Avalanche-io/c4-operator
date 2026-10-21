package telemetry

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
)

// KeySize is the farm key's length: 256 random bits.
const KeySize = 32

// Key is the farm's telemetry key and its epoch. It never leaves the farm:
// no record holds it, and the canary test looks for it in every output.
// Rotating it starts a new epoch, and pseudonyms from different epochs are
// unrelated.
type Key struct {
	secret [KeySize]byte
	Epoch  uint32
}

// NewKey is a key from its bytes and epoch.
func NewKey(secret []byte, epoch uint32) (Key, error) {
	var k Key
	if len(secret) != KeySize {
		return k, errors.New("telemetry key: want 32 bytes")
	}
	copy(k.secret[:], secret)
	k.Epoch = epoch
	return k, nil
}

// pseudonymSize is the truncated HMAC kept: 80 bits.
const pseudonymSize = 10

// Pseudonym is a name under the farm key: P(kind, name) =
// HMAC-SHA256(key, kind ‖ 0x00 ‖ name), truncated to 80 bits, with its
// rank, the name's position in sort order among the names of its kind in
// the same record. Nobody outside the farm can reverse one or test a
// guess; the rank tells only which of two unknown names sorts first, which
// is what the policy's tie-breaks need to replay.
type Pseudonym struct {
	Kind Kind
	sum  [pseudonymSize]byte
	Rank uint32
}

var pseudoEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Text is the pseudonym as written: its kind's letter, a dot and 16
// base32 characters.
func (p Pseudonym) Text() string {
	return p.Kind.prefix() + "." + pseudoEncoding.EncodeToString(p.sum[:])
}

// MarshalJSON writes {"id": text, "rank": n}.
func (p Pseudonym) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID   string `json:"id"`
		Rank uint32 `json:"rank"`
	}{p.Text(), p.Rank})
}

// Synthetic is a name that sorts as the original did among its kind: the
// kind's letter and the rank, zero-padded. A replay names everything this
// way, so every comparison the policy makes between names of one kind
// comes out as it did on the farm.
func (p Pseudonym) Synthetic() string {
	r := strconv.FormatUint(uint64(p.Rank), 10)
	for len(r) < 8 {
		r = "0" + r
	}
	return p.Kind.prefix() + r
}

func sum(k Key, kind Kind, name string) [pseudonymSize]byte {
	m := hmac.New(sha256.New, k.secret[:])
	m.Write([]byte{byte(kind >> 8), byte(kind), 0})
	m.Write([]byte(name))
	var out [pseudonymSize]byte
	copy(out[:], m.Sum(nil))
	return out
}

// Namer gives one record's names their pseudonyms and ranks. Every name is
// added first, then Freeze ranks them, then Of names each.
type Namer struct {
	key    Key
	names  map[Kind]map[string]bool
	ranks  map[Kind]map[string]uint32
	frozen bool
}

// NewNamer is a namer under key.
func NewNamer(key Key) *Namer {
	return &Namer{key: key, names: map[Kind]map[string]bool{}}
}

// Add notes a name of a kind. An empty name is none and gets no pseudonym.
func (n *Namer) Add(kind Kind, name string) {
	if name == "" || n.frozen {
		return
	}
	m := n.names[kind]
	if m == nil {
		m = map[string]bool{}
		n.names[kind] = m
	}
	m[name] = true
}

// Freeze ranks every name added, in byte order within its kind.
func (n *Namer) Freeze() {
	n.ranks = map[Kind]map[string]uint32{}
	for kind, m := range n.names {
		list := make([]string, 0, len(m))
		for s := range m {
			list = append(list, s)
		}
		sort.Strings(list)
		r := map[string]uint32{}
		for i, s := range list {
			r[s] = uint32(i)
		}
		n.ranks[kind] = r
	}
	n.frozen = true
}

// Of is a name's pseudonym. A name that was never added (or is empty) is
// the zero pseudonym, which names nothing.
func (n *Namer) Of(kind Kind, name string) Pseudonym {
	if name == "" {
		return Pseudonym{}
	}
	r, ok := n.ranks[kind][name]
	if !ok {
		return Pseudonym{}
	}
	return Pseudonym{Kind: kind, sum: sum(n.key, kind, name), Rank: r}
}

// Count is how many names of a kind the record has.
func (n *Namer) Count(kind Kind) int { return len(n.names[kind]) }

// None says the pseudonym names nothing.
func (p Pseudonym) None() bool { return p == Pseudonym{} }

// Resolve finds which of names has pseudonym p under key: the farm's own
// way back, for an admin on the farm with the key (c4 farm telemetry
// resolve). Nothing outside the farm can do this.
func Resolve(key Key, p Pseudonym, names []string) (string, bool) {
	for _, s := range names {
		if sum(key, p.Kind, s) == p.sum {
			return s, true
		}
	}
	return "", false
}
