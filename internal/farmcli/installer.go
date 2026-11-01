package farmcli

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
)

// SignedIn is the address that opens u, a page of c's HUD, signed in as
// the installer: on a local farm (c carries its installer token) a one-time
// code from POST /installer/code, which the HUD's /installer page trades
// for its cookie and then sends the browser on to u. The token itself is
// never in the address. A farm with no installer token, or a HUD from
// before them, opens u as it is.
func SignedIn(ctx context.Context, c *Client, u string) (string, error) {
	if !strings.HasPrefix(u, c.Base+"/") {
		return u, nil
	}
	code, err := InstallerCode(ctx, c)
	if err != nil || code == "" {
		return u, err
	}
	return c.Base + "/installer?" + url.Values{"code": {code}, "next": {strings.TrimPrefix(u, c.Base)}}.Encode(), nil
}

// InstallerCode is a one-time code the HUD's /installer page signs a
// browser in with, from POST /installer/code on c's installer token; ""
// when c carries none or the HUD is from before them.
func InstallerCode(ctx context.Context, c *Client) (string, error) {
	if c.Installer == "" {
		return "", nil
	}
	var r struct {
		Code string `json:"code"`
	}
	err := c.Do(ctx, "POST", "/installer/code", struct{}{}, &r)
	var ae *APIError
	switch {
	case errors.As(err, &ae) && ae.Status == http.StatusNotFound:
		return "", nil
	case err != nil:
		return "", err
	case r.Code == "":
		return "", errors.New("the HUD gave no code to sign the browser in with")
	}
	return r.Code, nil
}
