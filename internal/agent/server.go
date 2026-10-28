// Package agent is Albert's local HTTP API: the inventory, update and
// install jobs, search and a live event stream, behind token, host, origin
// and CSRF checks.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/kamllistos/albert/internal/inventory"
	"github.com/kamllistos/albert/internal/jobs"
	"github.com/kamllistos/albert/internal/provider"
)

// Config is the server's security and UI setup.
type Config struct {
	Token        string
	AllowedHosts []string // e.g. "localhost:7879","127.0.0.1:7879","localhost:7878","127.0.0.1:7878"
	UIOrigins    []string // "http://localhost:7879","http://127.0.0.1:7879"
	// UI is non-nil in native mode: it is served at "/". It never hands out
	// the token; the page trades a one-time code at POST /api/session (R20).
	UI      fs.FS
	Version string
	// Latest returns a newer Albert release version, or "" (may be nil).
	Latest func() string
}

var (
	// heartbeatInterval is how often an idle event stream gets ": ping".
	heartbeatInterval = 15 * time.Second
	// searchTimeout bounds each provider's search.
	searchTimeout = 10 * time.Second
	// eventWriteTimeout drops an event-stream client that stops reading.
	eventWriteTimeout = 30 * time.Second

	logf = log.Printf
)

const (
	minQuery = 2   // characters
	maxQuery = 100 // bytes
)

type server struct {
	cfg     Config
	inv     *inventory.Inventory
	jm      *jobs.Manager
	ctx     context.Context // background refreshes stop when it is done
	refresh coalescer
	refused refusalLog
	codes   loginCodes
	handler http.Handler
}

// NewServer returns the agent's HTTP handler.
func NewServer(cfg Config, inv *inventory.Inventory, jm *jobs.Manager) http.Handler {
	return newServer(context.Background(), cfg, inv, jm)
}

func newServer(ctx context.Context, cfg Config, inv *inventory.Inventory, jm *jobs.Manager) *server {
	s := &server{cfg: cfg, inv: inv, jm: jm, ctx: ctx}

	api := http.NewServeMux()
	api.HandleFunc("GET /api/health", s.health)
	api.HandleFunc("GET /api/tools", s.tools)
	api.HandleFunc("POST /api/refresh", s.refreshHandler)
	api.HandleFunc("POST /api/update", s.update)
	api.HandleFunc("POST /api/delete", s.deleteTool)
	api.HandleFunc("POST /api/install", s.install)
	api.HandleFunc("GET /api/search", s.search)
	api.HandleFunc("GET /api/jobs", s.jobList)
	api.HandleFunc("GET /api/jobs/{id}", s.job)
	api.HandleFunc("GET /api/events", s.events)
	api.HandleFunc("POST /api/login-code", s.loginCode)
	api.HandleFunc("POST "+sessionPath, s.session) // no token check: see guardAPI

	mux := http.NewServeMux()
	mux.Handle("/api/", s.guardAPI(api))
	if cfg.UI != nil {
		mux.Handle("/", s.uiHandler(http.FileServerFS(cfg.UI)))
	}
	s.handler = s.guardHost(mux)
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// wait blocks until background refreshes have stopped.
func (s *server) wait() { s.refresh.wait() }

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v) // escapes <, > and & too
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// readJSON decodes a single JSON object from the body into v, refusing
// unknown fields. It writes the error response itself and reports false on
// failure.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if err == nil && dec.Decode(&struct{}{}) != io.EOF {
		err = errors.New("trailing data")
	}
	if err == nil {
		return true
	}
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		writeError(w, http.StatusRequestEntityTooLarge, "This request is too large.")
	} else {
		writeError(w, http.StatusBadRequest, "Albert couldn't read this request.")
	}
	return false
}

func (s *server) health(w http.ResponseWriter, r *http.Request) {
	latest := ""
	if s.cfg.Latest != nil {
		latest = s.cfg.Latest()
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.cfg.Version, "latest": latest})
}

func (s *server) tools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.inv.Snapshot())
}

// refreshHandler rescans one source (?source=) or all of them, then checks
// for updates, in the background.
func (s *server) refreshHandler(w http.ResponseWriter, r *http.Request) {
	source := r.URL.Query().Get("source")
	if source != "" && !s.knownSource(w, source) {
		return
	}
	s.refresh.run(source, func() {
		if source == "" {
			s.inv.ScanAll(s.ctx)
		} else {
			_ = s.inv.Scan(s.ctx, source) // the error shows in the provider's status
		}
		s.inv.CheckOutdated(s.ctx, source)
	})
	writeJSON(w, http.StatusAccepted, map[string]bool{"ok": true})
}

// knownSource writes a 400 and reports false if source is not a provider.
func (s *server) knownSource(w http.ResponseWriter, source string) bool {
	if _, ok := s.inv.Provider(source); ok {
		return true
	}
	writeError(w, http.StatusBadRequest, fmt.Sprintf("Albert doesn't know a source called %q.", source))
	return false
}

// validNames writes a 400 and reports false if any name is unsafe.
func validNames(w http.ResponseWriter, names []string) bool {
	for _, n := range names {
		if !provider.ValidName(n) {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("%q isn't a valid tool name.", n))
			return false
		}
	}
	return true
}

type updateRequest struct {
	Source     string   `json:"source"`
	Names      []string `json:"names"`
	All        bool     `json:"all"`
	IncludePip bool     `json:"includePip"`
}

// update submits update jobs: for the named tools of one source, or, with
// all, one job per source that has something to update.
func (s *server) update(w http.ResponseWriter, r *http.Request) {
	var req updateRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.All {
		if req.Source != "" || len(req.Names) > 0 {
			writeError(w, http.StatusBadRequest, "Choose either all tools or specific tools, not both.")
			return
		}
		ids := []string{}
		for _, src := range inventory.UpdatableSources(s.inv.Snapshot(), req.IncludePip) {
			id, ok := s.submit(w, src, jobs.KindUpdate, nil)
			if !ok {
				return
			}
			ids = append(ids, id)
		}
		writeJSON(w, http.StatusOK, map[string][]string{"jobs": ids})
		return
	}
	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "Say which source the tools come from.")
		return
	}
	if !s.knownSource(w, req.Source) {
		return
	}
	if len(req.Names) == 0 {
		writeError(w, http.StatusBadRequest, "Choose at least one tool to update.")
		return
	}
	if !validNames(w, req.Names) {
		return
	}
	if id, ok := s.submit(w, req.Source, jobs.KindUpdate, req.Names); ok {
		writeJSON(w, http.StatusOK, map[string][]string{"jobs": {id}})
	}
}

type installRequest struct {
	Source string `json:"source"`
	Name   string `json:"name"`
}

type deleteRequest struct {
	Source string `json:"source"`
	Name   string `json:"name"`
}

func (s *server) install(w http.ResponseWriter, r *http.Request) {
	var req installRequest
	if !readJSON(w, r, &req) {
		return
	}

	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "Say which source to install from.")
		return
	}
	if !s.knownSource(w, req.Source) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "Enter the name of a tool to install.")
		return
	}
	if !validNames(w, []string{req.Name}) {
		return
	}
	if id, ok := s.submit(w, req.Source, jobs.KindInstall, []string{req.Name}); ok {
		writeJSON(w, http.StatusOK, map[string][]string{"jobs": {id}})
	}
}

func (s *server) deleteTool(w http.ResponseWriter, r *http.Request) {
	var req deleteRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Source == "" {
		writeError(w, http.StatusBadRequest, "Say which source the tool comes from.")
		return
	}
	if !s.knownSource(w, req.Source) {
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "Choose a tool to delete.")
		return
	}
	if !validNames(w, []string{req.Name}) {
		return
	}
	if id, ok := s.submit(w, req.Source, jobs.KindDelete, []string{req.Name}); ok {
		writeJSON(w, http.StatusOK, map[string][]string{"jobs": {id}})
	}
}

// submit queues a job whose input has already been checked, so a failure
// here means the agent cannot take jobs (it is shutting down).
func (s *server) submit(w http.ResponseWriter, source, kind string, names []string) (string, bool) {
	id, err := s.jm.Submit(source, kind, names)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "Albert couldn't start this job: "+err.Error())
		return "", false
	}
	return id, true
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(q) < minQuery {
		writeError(w, http.StatusBadRequest, "Type at least 2 characters to search.")
		return
	}
	if len(q) > maxQuery {
		writeError(w, http.StatusBadRequest, "That search is too long.")
		return
	}
	snap := s.inv.Snapshot()
	ps := make([]provider.Provider, 0, len(snap.Providers))
	for _, st := range snap.Providers {
		if p, ok := s.inv.Provider(st.Name); ok {
			ps = append(ps, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string][]provider.Result{
		"results": provider.SearchAll(r.Context(), ps, q, searchTimeout),
	})
}

func (s *server) jobList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]jobs.Job{"jobs": s.jm.List()})
}

func (s *server) job(w http.ResponseWriter, r *http.Request) {
	j, ok := s.jm.Get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, "Albert couldn't find that job.")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

// jobLine is the data of a "job" event.
type jobLine struct {
	ID   string `json:"id"`
	Line string `json:"line"`
}

// events streams Server-Sent Events: "inventory" (a Snapshot) on connect and
// after every change, "job" (a line of output) and "jobdone" (the finished
// Job), plus a ": ping" comment when idle. Both publishers drop or coalesce
// rather than wait, so a slow client never holds them up; a client that
// stops reading entirely is dropped after eventWriteTimeout.
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	invCh, unsubInv := s.inv.Subscribe()
	defer unsubInv()
	jobCh, unsubJobs := s.jm.Subscribe()
	defer unsubJobs()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)

	write := func(format string, args ...any) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(eventWriteTimeout)) // unsupported by some writers
		if _, err := fmt.Fprintf(w, format, args...); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	// send writes one event. data is JSON, so it never contains a newline
	// that could end the event early.
	send := func(event string, v any) bool {
		data, err := json.Marshal(v)
		if err != nil {
			logf("event %s: %v", event, err)
			return true
		}
		return write("event: %s\ndata: %s\n\n", event, data)
	}

	if !send("inventory", s.inv.Snapshot()) {
		return
	}
	ping := time.NewTicker(heartbeatInterval)
	defer ping.Stop()
	for {
		ok := true
		select {
		case <-r.Context().Done():
			return
		case _, open := <-invCh:
			if !open {
				return
			}
			ok = send("inventory", s.inv.Snapshot())
		case ev, open := <-jobCh:
			if !open {
				return // the job manager closed
			}
			if ev.Done {
				ok = send("jobdone", ev.Job)
			} else {
				ok = send("job", jobLine{ID: ev.JobID, Line: ev.Line})
			}
		case <-ping.C:
			ok = write(": ping\n\n")
		}
		if !ok {
			return
		}
	}
}

// coalescer runs at most one fn per key at a time. A call made while that
// key's fn is running schedules one more run after it; further calls in the
// meantime merge into that one.
type coalescer struct {
	mu    sync.Mutex
	state map[string]*coState
	wg    sync.WaitGroup
}

type coState struct{ again bool }

// run starts fn for key in a new goroutine, or, if key is busy, queues a
// rerun. It never blocks.
func (c *coalescer) run(key string, fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == nil {
		c.state = map[string]*coState{}
	}
	if st, busy := c.state[key]; busy {
		st.again = true
		return
	}
	st := &coState{}
	c.state[key] = st
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		for {
			fn()
			c.mu.Lock()
			if !st.again {
				delete(c.state, key)
				c.mu.Unlock()
				return
			}
			st.again = false
			c.mu.Unlock()
		}
	}()
}

// wait blocks until no fn is running.
func (c *coalescer) wait() { c.wg.Wait() }
