// Package web serves the noters API over Datastar's SSE hypermedia protocol.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/alexbathome/noter/internal/store"
	datastar "github.com/starfederation/datastar-go/datastar"
)

//go:embed board.tmpl
var templateFS embed.FS

var boardTmpl = template.Must(template.ParseFS(templateFS, "board.tmpl"))

// maxContent caps a single task body, so a runaway client cannot exhaust memory.
const maxContent = 1 << 20 // 1 MiB

// Server wires the store to the HTTP API.
type Server struct {
	store  *store.Store
	base   string // absolute origin the browser uses to reach this server
	static fs.FS  // the same UI GitHub Pages hosts, served locally too
	hub    *hub
}

// New builds the server. base is the origin browsers dial (e.g.
// http://localhost:11911) and is baked into the hypermedia the API returns,
// since the page itself may be served from GitHub Pages.
func New(s *store.Store, base string, static fs.FS) *Server {
	return &Server{store: s, base: strings.TrimSuffix(base, "/"), static: static, hub: newHub()}
}

// Handler returns the routed, CORS-wrapped handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Liveness probe the GitHub Pages shell uses to decide whether noters is
	// running before it boots the app.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true,"app":"noters"}`)
	})

	mux.HandleFunc("GET /api/board", s.streamBoard)
	mux.HandleFunc("GET /api/tasks/{id}", s.openTask)
	mux.HandleFunc("POST /api/tasks", s.createTask)
	mux.HandleFunc("PUT /api/tasks/{id}", s.saveTask)
	mux.HandleFunc("POST /api/tasks/{id}/move", s.moveTask)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.deleteTask)

	if s.static != nil {
		mux.Handle("GET /", http.FileServer(http.FS(s.static)))
	}
	return cors(mux)
}

// cors permits the GitHub Pages origin to call this loopback server. Datastar
// sends a Datastar-Request header, which makes every request non-simple and so
// triggers a preflight.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type, Datastar-Request")
		h.Set("Access-Control-Max-Age", "86400")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// streamBoard holds an SSE stream open, pushing the whole board on connect and
// again after every mutation from any client.
func (s *Server) streamBoard(w http.ResponseWriter, r *http.Request) {
	sse := datastar.NewSSE(w, r)
	changed := s.hub.subscribe()
	defer s.hub.unsubscribe(changed)

	if err := s.patchBoard(sse); err != nil {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changed:
			if err := s.patchBoard(sse); err != nil {
				return
			}
		}
	}
}

func (s *Server) patchBoard(sse *datastar.ServerSentEventGenerator) error {
	html, err := s.renderBoard()
	if err != nil {
		log.Printf("render board: %v", err)
		return err
	}
	return sse.PatchElements(html)
}

type zoneView struct {
	Name  string
	Tasks []store.Task
}

func (s *Server) renderBoard() (string, error) {
	grouped, err := s.store.ByZone()
	if err != nil {
		return "", err
	}
	zones := make([]zoneView, 0, len(store.Zones))
	for _, name := range store.Zones {
		zones = append(zones, zoneView{Name: name, Tasks: grouped[name]})
	}
	var buf strings.Builder
	err = boardTmpl.Execute(&buf, struct {
		Base  string
		Zones []zoneView
	}{Base: s.base, Zones: zones})
	return buf.String(), err
}

// openTask loads a task's body into the Monaco island. The editor is the one
// piece of genuinely client-side state, so the server drives it explicitly
// rather than trying to express it as markup.
func (s *Server) openTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.store.Get(r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	id, _ := json.Marshal(task.ID)
	content, _ := json.Marshal(task.Content)
	sse := datastar.NewSSE(w, r)
	sse.ExecuteScript(fmt.Sprintf("window.noter.load(%s,%s)", id, content))
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	zone := r.URL.Query().Get("zone")
	task, err := s.store.Create(zone)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.hub.broadcast()

	// Select the new task and focus the editor, so a fresh task is immediately
	// typeable exactly like the old client-only version.
	id, _ := json.Marshal(task.ID)
	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]string{"sel": task.ID})
	sse.ExecuteScript(fmt.Sprintf("window.noter.load(%s,\"\")", id))
}

func (s *Server) saveTask(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxContent))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.store.SetContent(r.PathValue("id"), string(body)); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.hub.broadcast()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) moveTask(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	index, err := strconv.Atoi(q.Get("index"))
	if err != nil {
		http.Error(w, "index must be an integer", http.StatusBadRequest)
		return
	}
	if err := s.store.Move(r.PathValue("id"), q.Get("zone"), index); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	s.hub.broadcast()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	if err := s.store.Delete(r.PathValue("id")); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.hub.broadcast()

	sse := datastar.NewSSE(w, r)
	sse.MarshalAndPatchSignals(map[string]string{"sel": ""})
	sse.ExecuteScript("window.noter.clear()")
}

// hub fans mutations out to every open board stream, so several tabs (or the
// local page and the GitHub Pages one) stay in step.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{subs: map[chan struct{}]struct{}{}} }

func (h *hub) subscribe() chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs[ch] = struct{}{}
	return ch
}

func (h *hub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subs, ch)
}

func (h *hub) broadcast() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default: // a refresh is already queued for this subscriber
		}
	}
}
