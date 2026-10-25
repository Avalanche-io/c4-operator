package server

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/hud/api"
)

// The installer token (API.md, "Development viewers and the installer
// token"). A development farm's viewers (?as=) are anyone's for the
// asking, so the HUD honours one only for a request that carries this
// machine's installer secret: the token itself (X-C4-Installer), or the
// cookie a browser gets for it through a single-use code (c4 farm open).
// Anything else that reaches the HUD, a unit's pod through the farm's
// Service among them, signs in as on any farm, or is no one.

// InstallerHeader carries the installer token.
const InstallerHeader = "X-C4-Installer"

// HUDHeader marks a write the HUD's own page sends: a browser sends no
// custom header across origins without a preflight, which the HUD never
// answers, so a write authorized by the cookie alone must carry it.
const HUDHeader = "X-C4-HUD"

const (
	// installerCodeTTL is how long a code from POST /installer/code
	// stays good.
	installerCodeTTL = 2 * time.Minute
	// installerCodesMax is how many codes may be outstanding at once.
	installerCodesMax = 8
	// installerCookieAge is the cookie's lifetime: it stops working
	// sooner when the token changes, since its value is derived from it.
	installerCookieAge = 365 * 24 * time.Hour
)

var (
	errNeedInstaller = errors.New("development viewers need this machine's installer token: run c4 farm open")
	errBrowserWrite  = forbiddenViewer("a write from the browser needs the HUD's own page")
)

// forbiddenViewer is a request whose viewer is known but may not do this
// (403).
type forbiddenViewer string

func (e forbiddenViewer) Error() string { return string(e) }

// Ways a request may use a development viewer (api.Viewer.DevBy): the
// installer's secret (installerAuth), or open development viewers.
const (
	byInstallerToken  = "token"
	byInstallerCookie = "cookie"
	// devOpen: open development viewers, no secret at all.
	devOpen = "open"
)

// installerAuth says how r carries this machine's installer secret: the
// token in X-C4-Installer, this farm's installer cookie, or "" for
// neither (or a HUD with no installer token).
func (s *Server) installerAuth(r *http.Request) string {
	if len(s.InstallerToken) == 0 {
		return ""
	}
	if h := r.Header.Get(InstallerHeader); h != "" && subtle.ConstantTimeCompare([]byte(h), s.InstallerToken) == 1 {
		return byInstallerToken
	}
	if c, err := r.Cookie(s.installerCookieName()); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.installerCookieValue())) == 1 {
		return byInstallerCookie
	}
	return ""
}

// installerGuarded says the HUD's development viewers are this machine's
// alone: it has an installer token, and is not open to anyone.
func (s *Server) installerGuarded() bool {
	return len(s.InstallerToken) > 0 && !s.DevViewersOpen
}

// installerCookieName is this farm's cookie: c4_installer_ and the first
// 8 hex digits of the token's SHA-256, so two farms on one machine keep
// theirs apart. The HUD reads no other.
func (s *Server) installerCookieName() string {
	sum := sha256.Sum256(s.InstallerToken)
	return "c4_installer_" + hex.EncodeToString(sum[:4])
}

// installerCookieValue is derived from the token, never the token.
func (s *Server) installerCookieValue() string {
	m := hmac.New(sha256.New, s.InstallerToken)
	m.Write([]byte("c4 installer cookie"))
	return hex.EncodeToString(m.Sum(nil))
}

// devViewer is r's development viewer: nil, nil when r takes none and
// signs in as on any farm. Development viewers need DevViewersOpen or
// the installer's secret; a request that names one (?as=) without either
// is refused rather than passed on, and one that names none falls
// through, never to a viewer by default.
func (s *Server) devViewer(r *http.Request) (*api.Viewer, error) {
	if s.DevViewers == nil {
		return nil, nil
	}
	as := r.URL.Query().Get("as")
	how := s.installerAuth(r)
	switch {
	case s.DevViewersOpen:
		if how == "" {
			how = devOpen
		}
	case how == "" && as != "":
		return nil, errNeedInstaller
	case how == "":
		return nil, nil
	case how == byInstallerCookie && isWrite(r.Method) && r.Header.Get(HUDHeader) != "1" && r.Header.Get(InstallerHeader) == "":
		return nil, errBrowserWrite
	}
	v, ok := s.DevViewers[as]
	if !ok && as != "" {
		names := make([]string, 0, len(s.DevViewers))
		for n := range s.DevViewers {
			names = append(names, n)
		}
		slices.Sort(names)
		return nil, badViewer(fmt.Sprintf("no development viewer %q here: ?as= one of %s", as, strings.Join(names, ", ")))
	}
	if !ok {
		v, ok = s.DevViewers["wrangler"]
	}
	if !ok {
		return nil, nil
	}
	// A copy, marked: the shared viewer stays as configured.
	out := *s.expand(v)
	out.DevBy = how
	return &out, nil
}

// isDev says v is a development viewer.
func isDev(v *api.Viewer) bool { return v != nil && v.DevBy != "" }

// byInstaller says v is a development viewer on the installer's secret:
// the installer's word, graded claimed.
func byInstaller(v *api.Viewer) bool {
	return v != nil && (v.DevBy == byInstallerToken || v.DevBy == byInstallerCookie)
}

// singlePersonFlag is --single-person-farm where the HUD honours it: on
// a HUD with an installer token, or with open development viewers
// (tests). Elsewhere a name on the command line makes no one every role.
func (s *Server) singlePersonFlag() string {
	if len(s.InstallerToken) == 0 && !s.DevViewersOpen {
		return ""
	}
	return s.SinglePerson
}

func isWrite(method string) bool {
	return method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
}

// handleInstallerCode answers POST /api/v1/installer/code: a single-use
// code, good for two minutes, that a browser trades at /installer for
// the installer cookie. Only the token itself asks for one.
func (s *Server) handleInstallerCode(w http.ResponseWriter, r *http.Request) {
	if s.installerAuth(r) != byInstallerToken {
		fail(w, http.StatusUnauthorized, "a code for the browser needs this machine's installer token (%s): run c4 farm open", InstallerHeader)
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		fail(w, http.StatusInternalServerError, "%v", err)
		return
	}
	code := hex.EncodeToString(b)
	now := s.now()
	s.installerMu.Lock()
	if s.installerCodes == nil {
		s.installerCodes = map[string]time.Time{}
	}
	for c, until := range s.installerCodes {
		if !now.Before(until) {
			delete(s.installerCodes, c)
		}
	}
	for len(s.installerCodes) >= installerCodesMax {
		// the oldest goes first
		oldest, at := "", time.Time{}
		for c, until := range s.installerCodes {
			if oldest == "" || until.Before(at) {
				oldest, at = c, until
			}
		}
		delete(s.installerCodes, oldest)
	}
	s.installerCodes[code] = now.Add(installerCodeTTL)
	s.installerMu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"code": code})
}

// handleInstaller answers GET /installer?code=C&next=N: a good, unused
// code sets the installer cookie and sends the browser on to N, a path on
// this HUD (its #/ route kept), or to / for anything else.
func (s *Server) handleInstaller(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	now := s.now()
	s.installerMu.Lock()
	until, ok := s.installerCodes[code]
	if ok {
		delete(s.installerCodes, code)
	}
	s.installerMu.Unlock()
	if len(s.InstallerToken) == 0 || code == "" || !ok || !now.Before(until) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprintln(w, "This code is not valid: it is unknown, used or expired. Run c4 farm open again.")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: s.installerCookieName(), Value: s.installerCookieValue(), Path: "/",
		MaxAge: int(installerCookieAge / time.Second), HttpOnly: true, SameSite: http.SameSiteStrictMode})
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Location", safeNext(r.URL.Query().Get("next")))
	w.WriteHeader(http.StatusSeeOther)
}

// safeNext is where /installer may send a browser: a path on this HUD,
// never another host ("//host", "/\host", "https://..."), nor anything a
// browser would read as one once it drops tabs and newlines.
func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	return next
}

func (s *Server) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}
