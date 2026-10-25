package farmcli

import (
	"context"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// Bind binds the farm to its owner (POST /farm/binding), as the caller,
// its installer: the farm's records accrue to the owner from then on. A
// farm already bound to the same owner answers Existing and changes
// nothing; one bound to someone else is refused, with how an owner
// changes.
func Bind(ctx context.Context, c *Client, req api.FarmBindingRequest) (api.FarmBindingResponse, error) {
	var r api.FarmBindingResponse
	err := c.Do(ctx, "POST", "/farm/binding", req, &r)
	return r, err
}
