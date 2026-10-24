// Package jobs runs tool updates and installs in the background, one at a
// time per source (brew and cask share one queue), and streams their output
// to subscribers.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/kamllistos/albert/internal/inventory"
	"github.com/kamllistos/albert/internal/provider"
)

// Job kinds and statuses.
const (
	KindUpdate  = "update"
	KindInstall = "install"

	StatusQueued  = "queued"
	StatusRunning = "running"
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

const (
	// maxJobs is how many jobs are kept. Only finished jobs are evicted,
	// oldest first.
	maxJobs = 50
	// subBuffer is each subscriber's channel size.
	subBuffer = 256
)

// Job is one update or install.
type Job struct {
	ID      string    `json:"id"`
	Source  string    `json:"source"`
	Kind    string    `json:"kind"`
	Status  string    `json:"status"`
	Output  string    `json:"output"`
	Message string    `json:"message"`
	Targets []string  `json:"targets"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended"`
}

// Event is one line of output, or the end of a job.
type Event struct {
	JobID string `json:"jobId"`
	Line  string `json:"line"`
	Done  bool   `json:"done"`
	Job   *Job   `json:"job"` // set when Done
}

// Manager runs jobs. It is safe for concurrent use.
type Manager struct {
	inv    *inventory.Inventory
	ctx    context.Context // cancelled by Close; jobs have no other timeout
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// mu guards everything below, including each Job and the subscriber
	// channels (so output order, Done and unsubscribe are all consistent).
	mu      sync.Mutex
	closed  bool
	jobs    map[string]*Job
	order   []string          // oldest first
	queues  map[string]*queue // by queueKey(source)
	subs    map[int]chan Event
	nextSub int
}

// queue is one FIFO of waiting jobs. running is true while its worker
// goroutine exists.
type queue struct {
	pending []*Job
	running bool
}

// NewManager returns a manager that uses inv to find providers and to
// refresh the inventory after each job.
func NewManager(inv *inventory.Inventory) *Manager {
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		inv:    inv,
		ctx:    ctx,
		cancel: cancel,
		jobs:   map[string]*Job{},
		queues: map[string]*queue{},
		subs:   map[int]chan Event{},
	}
}

// Close cancels every running job, fails the queued ones, waits for them to
// stop and closes all subscriber channels. Submit fails after Close.
func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	m.cancel()
	m.wg.Wait()

	m.mu.Lock()
	defer m.mu.Unlock()
	for id, ch := range m.subs {
		close(ch)
		delete(m.subs, id)
	}
}

// Submit queues a job and returns its ID. kind is "update" (no names means
// everything the source has) or "install" (exactly one name).
func (m *Manager) Submit(source, kind string, names []string) (string, error) {
	if _, ok := m.inv.Provider(source); !ok {
		return "", fmt.Errorf("unknown source %q", source)
	}
	switch kind {
	case KindUpdate:
	case KindInstall:
		if len(names) != 1 {
			return "", errors.New("install needs exactly one tool name")
		}
	default:
		return "", fmt.Errorf("unknown job kind %q", kind)
	}
	for _, n := range names {
		if !provider.ValidName(n) {
			return "", fmt.Errorf("invalid tool name %q", n)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return "", errors.New("albert is shutting down")
	}
	id, err := m.newIDLocked()
	if err != nil {
		return "", err
	}
	j := &Job{
		ID:      id,
		Source:  source,
		Kind:    kind,
		Status:  StatusQueued,
		Targets: append([]string{}, names...),
	}
	m.jobs[id] = j
	m.order = append(m.order, id)
	m.pruneLocked()

	key := queueKey(source)
	q := m.queues[key]
	if q == nil {
		q = &queue{}
		m.queues[key] = q
	}
	q.pending = append(q.pending, j)
	if !q.running {
		q.running = true
		m.wg.Add(1)
		go m.work(q)
	}
	return id, nil
}

// Get returns a copy of the job.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return copyJob(j), true
}

// List returns copies of the kept jobs, newest first.
func (m *Manager) List() []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		out = append(out, copyJob(m.jobs[m.order[i]]))
	}
	return out
}

// Subscribe returns a channel of events for all jobs. Sending never blocks
// a job: the channel holds 256 events. When it is full a new line event is
// dropped; a Done event instead evicts the oldest queued line to make room.
// So a slow subscriber can miss lines (the job's Output has them all) but
// never a Done event. The returned func
// unsubscribes and closes the channel; it is safe to call more than once.
func (m *Manager) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, subBuffer)
	m.mu.Lock()
	id := m.nextSub
	m.nextSub++
	if m.closed {
		close(ch)
	} else {
		m.subs[id] = ch
	}
	m.mu.Unlock()
	return ch, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if c, ok := m.subs[id]; ok {
			delete(m.subs, id)
			close(c)
		}
	}
}

// queueKey is the queue a source's jobs wait in. Formulae and casks both run
// Homebrew, which refuses to run twice at once, so they share one queue.
func queueKey(source string) string {
	if source == "cask" {
		return "brew"
	}
	return source
}

// work runs one queue's jobs, oldest first, until none are left.
func (m *Manager) work(q *queue) {
	defer m.wg.Done()
	for {
		m.mu.Lock()
		if len(q.pending) == 0 {
			q.running = false
			m.mu.Unlock()
			return
		}
		j := q.pending[0]
		q.pending = q.pending[1:]
		m.mu.Unlock()
		m.run(j)
	}
}

// run executes one job and reports it done.
func (m *Manager) run(j *Job) {
	m.mu.Lock()
	j.Status = StatusRunning
	j.Started = time.Now()
	source, kind := j.Source, j.Kind
	targets := append([]string(nil), j.Targets...)
	m.mu.Unlock()

	status, msg := StatusSuccess, ""
	if err := m.exec(j, source, kind, targets); err != nil {
		status, msg = StatusFailed, m.failMessage(j, kind, targets, err)
	}

	// Refresh before reporting so a client that reloads on Done sees fresh
	// data. Skipped on shutdown: the context is cancelled and it would only
	// return at once.
	if m.ctx.Err() == nil {
		_ = m.inv.Scan(m.ctx, source)
		m.inv.CheckOutdated(m.ctx, source)
	}

	m.mu.Lock()
	j.Status, j.Message, j.Ended = status, msg, time.Now()
	done := copyJob(j)
	m.emitLocked(Event{JobID: j.ID, Done: true, Job: &done})
	m.mu.Unlock()
}

// exec calls the provider, streaming its output into the job.
func (m *Manager) exec(j *Job, source, kind string, targets []string) (err error) {
	if err := m.ctx.Err(); err != nil {
		return err
	}
	p, ok := m.inv.Provider(source)
	if !ok {
		return fmt.Errorf("unknown source %q", source)
	}
	w := &lineWriter{m: m, j: j}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%s crashed: %v", source, r)
		}
		w.flush()
	}()
	if kind == KindInstall {
		return p.Install(m.ctx, targets[0], w)
	}
	return p.Upgrade(m.ctx, targets, w)
}

func (m *Manager) output(j *Job) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return j.Output
}

// failMessage is what a user is told when a job returned err.
// Password text in the output only matters when the provider failed.
func (m *Manager) failMessage(j *Job, kind string, targets []string, err error) string {
	switch {
	case errors.Is(err, provider.ErrReadOnly), errors.Is(err, provider.ErrUnsupported):
		return err.Error()
	case needsPassword(m.output(j)):
		return passwordMessage(j.Source, kind, targets)
	case m.ctx.Err() != nil:
		return "Albert stopped before this finished."
	}
	return err.Error()
}

func needsPassword(out string) bool {
	return strings.Contains(out, "sudo:") ||
		strings.Contains(out, "a terminal is required") ||
		strings.Contains(out, "Password:")
}

// passwordMessage tells the user to finish the job in Terminal: "albert
// install" for installs, "albert update" for updates. An update-all job
// names its source, since that is all the job updated; pip also needs
// --include-pip, without which "update --all" skips it.
func passwordMessage(source, kind string, targets []string) string {
	verb := "update"
	if kind == KindInstall {
		verb = "install"
	}
	tool := "--all --source " + source
	if source == "pip" {
		tool += " --include-pip"
	}
	if len(targets) > 0 {
		tool = targets[0]
	}
	return fmt.Sprintf(`This needs your password. Run "albert %s %s" in Terminal.`, verb, tool)
}

// lineWriter is the io.Writer given to providers. It appends to the job's
// Output and emits an Event for every complete line.
type lineWriter struct {
	m       *Manager
	j       *Job
	partial string // bytes after the last '\n'
}

func (w *lineWriter) Write(p []byte) (int, error) {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	w.j.Output += string(p)
	w.partial += string(p)
	for {
		i := strings.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		w.m.emitLocked(Event{JobID: w.j.ID, Line: w.partial[:i]})
		w.partial = w.partial[i+1:]
	}
	return len(p), nil
}

// flush emits a trailing line that had no newline.
func (w *lineWriter) flush() {
	w.m.mu.Lock()
	defer w.m.mu.Unlock()
	if w.partial != "" {
		w.m.emitLocked(Event{JobID: w.j.ID, Line: w.partial})
		w.partial = ""
	}
}

// emitLocked sends ev to every subscriber without blocking. m.mu must be
// held; being the only sender, it can safely make room in a full channel.
func (m *Manager) emitLocked(ev Event) {
	for _, ch := range m.subs {
		select {
		case ch <- ev:
			continue
		default:
		}
		if !ev.Done {
			continue // full: this line is dropped
		}
		makeRoom(ch)
		select {
		case ch <- ev:
		default: // unreachable: we are the only sender and just made room
		}
	}
}

// makeRoom frees at least one slot in a full channel by dropping its oldest
// output line, so that Done events survive. If the channel holds nothing but
// Done events, the oldest of those goes instead.
func makeRoom(ch chan Event) {
	var kept []Event
	dropped := false
drain:
	for {
		select {
		case e := <-ch:
			if !dropped && !e.Done {
				dropped = true
				continue
			}
			kept = append(kept, e)
		default:
			break drain
		}
	}
	if !dropped && len(kept) >= cap(ch) {
		kept = kept[1:]
	}
	for _, e := range kept {
		select {
		case ch <- e:
		default: // a slot is always free: we are the only sender
		}
	}
}

// newIDLocked returns an unused 8-hex-char ID.
func (m *Manager) newIDLocked() (string, error) {
	for range 10 {
		var b [4]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", err
		}
		id := hex.EncodeToString(b[:])
		if _, taken := m.jobs[id]; !taken {
			return id, nil
		}
	}
	return "", errors.New("could not make a job ID")
}

// pruneLocked drops the oldest finished jobs beyond maxJobs.
func (m *Manager) pruneLocked() {
	for len(m.order) > maxJobs {
		idx := -1
		for i, id := range m.order {
			if s := m.jobs[id].Status; s == StatusSuccess || s == StatusFailed {
				idx = i
				break
			}
		}
		if idx < 0 {
			return // everything is still active
		}
		delete(m.jobs, m.order[idx])
		m.order = append(m.order[:idx], m.order[idx+1:]...)
	}
}

func copyJob(j *Job) Job {
	c := *j
	c.Targets = append([]string{}, j.Targets...)
	return c
}
