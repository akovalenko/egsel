package core

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Slot is the switched resource one selector owns — e.g. the default
// route in a policy table, or the upstream of a local SOCKS front.
// Implementations must be idempotent; Read is the source of truth for
// the current owner (the engine never trusts its own memory over it).
type Slot interface {
	// Read reports the slot's current owner, "" when the slot is empty.
	// The note is a transport-side observation worth surfacing (e.g.
	// "shadowed by a lower-metric foreign default"), "" when all is well.
	Read(ctx context.Context) (owner, note string, err error)
	Activate(ctx context.Context, name string) error
	Clear(ctx context.Context) error
	// Describe names the slot for logs and status output.
	Describe() string
}

// ProbeResult is one probe verdict from a transport prober.
type ProbeResult struct {
	Cand string
	OK   bool
	RTT  time.Duration
	Err  string // failure detail, for status output only
	At   time.Time
}

type EngineConfig struct {
	Name       string // selector name (config key)
	Roster     []Candidate
	Health     HealthConfig
	Window     time.Duration // cross-prio upgrade soak
	SeedBudget time.Duration
	FailMode   FailMode
	LossTol    float64
	// ReconcileEvery paces the periodic reconcile; probe transitions
	// additionally trigger one immediately (instant failover).
	ReconcileEvery time.Duration
}

// Engine runs one selector: it folds probe results into per-candidate
// health, reads the slot from the kernel of truth, asks Decide, and
// applies the outcome. It is level-triggered throughout — a crashed
// daemon leaves the slot as-is, and a restart adopts it (see Decide).
type Engine struct {
	cfg   EngineConfig
	slot  Slot
	store *Store
	log   *slog.Logger
	now   func() time.Time

	mu       sync.Mutex // everything below; also serializes vs Snapshot
	trackers map[string]*Tracker

	current      string // owner per last successful Slot.Read
	opaque       bool
	currentSince time.Time
	lastDecision string
	lastSlotErr  string
	slotNote     string

	slotEmpty    bool
	seedDeadline time.Time
	pendingOwner string // target of our last successful Activate, until Read confirms
	loggedOpaque string
	readOnce     bool
}

func NewEngine(cfg EngineConfig, slot Slot, store *Store, log *slog.Logger, now func() time.Time) *Engine {
	e := &Engine{
		cfg:      cfg,
		slot:     slot,
		store:    store,
		log:      log.With("selector", cfg.Name),
		now:      now,
		trackers: map[string]*Tracker{},
	}
	for _, c := range cfg.Roster {
		prior := store.Prior(cfg.Name, c.Name)
		if !prior.IsZero() {
			e.log.Info("restored healthy-since from state file", "candidate", c.Name, "since", prior)
		}
		e.trackers[c.Name] = NewTracker(cfg.Health, prior)
	}
	return e
}

// Run consumes probe results and reconciles until ctx is done. All
// engine work happens on this goroutine; Snapshot is the only cross-
// goroutine reader.
func (e *Engine) Run(ctx context.Context, results <-chan ProbeResult) {
	tick := time.NewTicker(e.cfg.ReconcileEvery)
	defer tick.Stop()
	e.Reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case r := <-results:
			if e.observe(r) {
				e.Reconcile(ctx)
			}
		case <-tick.C:
			e.Reconcile(ctx)
		}
	}
}

// observe folds one probe result in; reports whether a verdict changed.
func (e *Engine) observe(r ProbeResult) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	t := e.trackers[r.Cand]
	if t == nil {
		return false
	}
	before := t.State().Status
	if !t.Observe(r) {
		return false
	}
	st := t.State()
	e.log.Info("verdict", "candidate", r.Cand, "was", before.String(), "now", st.Status.String(), "err", r.Err)
	switch st.Status {
	case Alive:
		e.store.SetHealthy(e.cfg.Name, r.Cand, st.AliveSince)
	case Dead:
		e.store.ClearHealthy(e.cfg.Name, r.Cand)
	}
	return true
}

// Reconcile does one level-triggered pass: read the slot, decide, act.
func (e *Engine) Reconcile(ctx context.Context) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()

	owner, note, err := e.slot.Read(ctx)
	if err != nil {
		if err.Error() != e.lastSlotErr {
			e.log.Error("slot read failed; no decisions until it recovers", "err", err)
			e.lastSlotErr = err.Error()
		}
		return
	}
	e.lastSlotErr = ""
	if note != e.slotNote {
		if note != "" {
			e.log.Warn("slot", "note", note)
		} else {
			e.log.Info("slot note cleared", "was", e.slotNote)
		}
		e.slotNote = note
	}
	e.noteOwner(owner, now)

	in := Input{
		Current:      e.current,
		Opaque:       e.opaque,
		Roster:       e.cfg.Roster,
		States:       e.states(),
		Window:       e.cfg.Window,
		SeedDeadline: e.seedDeadline,
		FailMode:     e.cfg.FailMode,
		LossTol:      e.cfg.LossTol,
		Now:          now,
	}
	d := Decide(in)
	e.lastDecision = d.Reason

	switch d.Op {
	case Activate:
		if d.Target == e.current {
			return
		}
		e.log.Info("switching slot", "to", d.Target, "reason", d.Reason)
		if err := e.slot.Activate(ctx, d.Target); err != nil {
			e.log.Error("slot activate failed; will retry", "to", d.Target, "err", err)
			return
		}
		e.pendingOwner = d.Target
		e.noteOwner(d.Target, now)
	case Clear:
		e.log.Info("clearing slot", "reason", d.Reason)
		if err := e.slot.Clear(ctx); err != nil {
			e.log.Error("slot clear failed; will retry", "err", err)
			return
		}
		e.pendingOwner = ""
		e.noteOwner("", now)
	}
}

// noteOwner tracks the observed slot owner: classifies it against the
// roster, arms the seed deadline on an empty-slot episode, and logs
// changes we did not make ourselves (adoption).
func (e *Engine) noteOwner(owner string, now time.Time) {
	opaque := false
	if owner != "" && !e.inRoster(owner) {
		opaque = true
		if e.loggedOpaque != owner {
			e.log.Warn("slot owned by device outside the roster; holding it (cannot assess or evict it)", "owner", owner)
			e.loggedOpaque = owner
		}
	}

	if owner == "" {
		if !e.slotEmpty || !e.readOnce {
			e.slotEmpty = true
			e.seedDeadline = now.Add(e.cfg.SeedBudget)
		}
	} else {
		e.slotEmpty = false
	}

	if !e.readOnce || owner != e.current {
		if owner != e.pendingOwner {
			e.log.Info("slot owner observed", "owner", orNone(owner), "adopted", true)
		}
		e.current, e.opaque, e.currentSince = owner, opaque, now
		if owner == e.pendingOwner {
			e.pendingOwner = ""
		}
	}
	e.readOnce = true
}

func (e *Engine) inRoster(name string) bool {
	for _, c := range e.cfg.Roster {
		if c.Name == name {
			return true
		}
	}
	return false
}

func (e *Engine) states() map[string]CandState {
	m := make(map[string]CandState, len(e.trackers))
	for n, t := range e.trackers {
		m[n] = t.State()
	}
	return m
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
