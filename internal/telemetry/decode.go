package telemetry

import (
	"encoding/json"
	"errors"
	"strings"
)

// Decode reads a record Encode wrote, for a replay. An enum name not in
// its table reads as other.
func Decode(data []byte, r Record) error { return json.Unmarshal(data, r) }

func decodeEnum(b []byte, table []string) (uint16, error) {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return 0, err
	}
	return uint16(enumIndex(table, s)), nil
}

func (k *Kind) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, kindTable)
	*k = Kind(v)
	return err
}

func (t *Tier) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, tierTable)
	*t = Tier(v)
	return err
}

func (r *Reason) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, reasonTable)
	*r = Reason(v)
	return err
}

func (v *Via) UnmarshalJSON(b []byte) error {
	x, err := decodeEnum(b, viaTable)
	*v = Via(x)
	return err
}

func (c *Channel) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, channelTable)
	*c = Channel(v)
	return err
}

func (p *Planner) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, plannerTable)
	*p = Planner(v)
	return err
}

func (s *Surplus) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, surplusTable)
	*s = Surplus(v)
	return err
}

func (d *Divergence) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, divergenceTable)
	*d = Divergence(v)
	return err
}

func (g *GOOS) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, goosTable)
	*g = GOOS(v)
	return err
}

func (g *GOARCH) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, goarchTable)
	*g = GOARCH(v)
	return err
}

func (g *GOAMD64) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, goamd64Table)
	*g = GOAMD64(v)
	return err
}

// UnmarshalJSON reads what Pseudonym.MarshalJSON writes.
func (p *Pseudonym) UnmarshalJSON(b []byte) error {
	var v struct {
		ID   string `json:"id"`
		Rank uint32 `json:"rank"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	prefix, enc, ok := strings.Cut(v.ID, ".")
	if !ok {
		return errors.New("pseudonym: no kind prefix")
	}
	kind := Kind(0)
	for i, pre := range kindPrefix {
		if pre == prefix {
			kind = Kind(i)
		}
	}
	raw, err := pseudoEncoding.DecodeString(enc)
	if err != nil || len(raw) != pseudonymSize {
		return errors.New("pseudonym: not 80 bits of base32")
	}
	*p = Pseudonym{Kind: kind, Rank: v.Rank}
	copy(p.sum[:], raw)
	if kind == 0 && v.ID == (Pseudonym{}).Text() {
		*p = Pseudonym{}
	}
	return nil
}

func (p *PolicyKind) UnmarshalJSON(b []byte) error {
	v, err := decodeEnum(b, policyTable)
	*p = PolicyKind(v)
	return err
}
