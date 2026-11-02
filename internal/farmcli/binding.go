package farmcli

import (
	"context"

	"github.com/Avalanche-io/c4-operator/hud/api"
	"github.com/Avalanche-io/c4-operator/internal/keystore"
)

// Bind binds the farm to its owner (POST /farm/binding), as the caller,
// its installer: the farm's records accrue to the owner from then on. A
// farm already bound to the same owner answers Existing and changes
// nothing; one bound to someone else is refused, with how an owner
// changes.
//
// The binding is the owner's first act (PEOPLE-KEYS-2 3.3), on the farm
// itself, which takes a touch: signed when c.Acts can sign it.
// The same binding again is read, not signed, so up run again asks for
// no touch.
func Bind(ctx context.Context, c *Client, req api.FarmBindingRequest) (api.FarmBindingResponse, error) {
	var r api.FarmBindingResponse
	if err := c.Do(ctx, "GET", "/farm/binding", nil, &r); err == nil && r.Binding != nil {
		b := r.Binding
		if b.Owner == req.Owner && b.To == req.To && b.RunFor == req.RunFor && b.OnePerson == req.OnePerson {
			r.Existing = true
			return r, nil
		}
	}
	r = api.FarmBindingResponse{}
	err := c.DoAct(ctx, "POST", "/farm/binding", "bind", FarmActOn(ctx, c), req, &r)
	return r, err
}

// FarmActOn is the farm itself as an act names it (farm:<farm ID>), from
// its people's issuer; nil when it has none.
// It is the farm this machine's key was certified at, from its own chain,
// so an act's subject and its farm are the same, whatever the HUD says.
func FarmActOn(ctx context.Context, c *Client) *api.ActOn {
	if c.Acts == nil {
		return nil
	}
	cert, err := LoadCert(c.Acts.Store, keystore.Device)
	if err != nil || cert == nil {
		return nil
	}
	f, err := certFarm(cert)
	if err != nil {
		return nil
	}
	return &api.ActOn{Name: "farm:" + f, Digest: map[string]string{"c4": f}}
}
