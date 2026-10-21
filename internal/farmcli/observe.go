package farmcli

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Avalanche-io/c4-operator/internal/hud"
)

// observed is GET /api/v1/observed: this machine's sessions, read-only,
// each row as c4-agent observe gives it.
type observed struct {
	Now      time.Time         `json:"now"`
	Machine  string            `json:"machine"`
	Sessions []json.RawMessage `json:"sessions"`
}

// SessionsServer is `c4 farm agents --open`: the HUD's sessions view on
// this machine, served on loopback with nothing behind it but `claude
// agents`. It reads only, and a minimal snapshot lets the app start.
type SessionsServer struct {
	Agent, Claude string
	All           bool
	Now           func() time.Time
	// Token is the run's secret: the API answers only a request that
	// carries it (as ?token= once, then the cookie that sets), so another
	// user of this machine or a web page cannot read the sessions. Serve
	// makes one.
	Token string
}

// tokenCookie carries the run's token after the first page load.
const tokenCookie = "c4-agents"

func (s SessionsServer) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// snapshot is just enough of the farm's snapshot for the HUD to start.
func (s SessionsServer) snapshot() map[string]any {
	user := os.Getenv("USER")
	return map[string]any{"now": s.now().UTC(), "viewer": map[string]any{"user": user, "projects": []string{}, "allProjects": true},
		"farm": map[string]any{"slotsUsed": 0, "slotsTotal": 0}, "jobs": []any{}, "pools": []any{}, "nodes": []any{},
		"classes": []any{}, "projects": []any{}, "incidents": []any{}}
}

// Handler serves the HUD's web files, GET /api/v1/observed, and the
// minimal snapshot (plain and as the event stream).
func (s SessionsServer) Handler() (http.Handler, error) {
	web, err := fs.Sub(hud.Web, "web")
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(web))
	mux.HandleFunc("GET /api/v1/observed", func(w http.ResponseWriter, r *http.Request) {
		rows, err := readSessions(r.Context(), SessionsArgs{Agent: s.Agent, Claude: s.Claude, All: s.All})
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		host, _ := os.Hostname()
		doc := observed{Now: s.now().UTC(), Machine: host, Sessions: make([]json.RawMessage, 0, len(rows))}
		for _, row := range rows {
			doc.Sessions = append(doc.Sessions, private(row.Raw))
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(doc)
	})
	mux.HandleFunc("GET /api/v1/snapshot", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.snapshot())
	})
	mux.HandleFunc("GET /api/v1/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		for {
			b, _ := json.Marshal(s.snapshot())
			fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", b)
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	})
	// anything else the HUD asks of a farm: there is none here
	mux.HandleFunc("GET /api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]string{"error": "no farm here: c4 farm agents --open shows this machine's sessions only"})
	})
	return s.guard(mux), nil
}

// guard lets a request through only from this machine's loopback names,
// and an API request only with the run's token: the page's ?token= sets a
// cookie the HUD's own requests then carry.
func (s SessionsServer) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "::1" {
			http.Error(w, "c4 farm agents answers only on this machine", http.StatusForbidden)
			return
		}
		ok := s.Token == ""
		if t := r.URL.Query().Get("token"); t != "" && subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) == 1 {
			http.SetCookie(w, &http.Cookie{Name: tokenCookie, Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			ok = true
		}
		if c, err := r.Cookie(tokenCookie); err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.Token)) == 1 {
			ok = true
		}
		if !ok && strings.HasPrefix(r.URL.Path, "/api/") {
			http.Error(w, "open the link c4 farm agents --open printed: it carries this run's token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// private is an observed row as the HUD may have it: its folder from the
// home folder (~/...), and no transcript path.
func private(raw json.RawMessage) json.RawMessage {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	delete(m, "transcript")
	if cwd, ok := m["cwd"].(string); ok {
		if home, err := os.UserHomeDir(); err == nil && home != "" && (cwd == home || strings.HasPrefix(cwd, home+string(filepath.Separator))) {
			m["cwd"] = "~" + filepath.ToSlash(cwd[len(home):])
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// Serve listens on 127.0.0.1:port (0 picks one), calls ready with the
// sessions view's URL, and serves until ctx ends.
func (s SessionsServer) Serve(ctx context.Context, port int, ready func(url string)) error {
	if s.Token == "" {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			return err
		}
		s.Token = hex.EncodeToString(b)
	}
	h, err := s.Handler()
	if err != nil {
		return err
	}
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Close()
	}()
	ready(fmt.Sprintf("http://%s/?token=%s#/sessions", l.Addr(), s.Token))
	if err := srv.Serve(l); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
