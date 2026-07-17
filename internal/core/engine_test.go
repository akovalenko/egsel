package core

import (
	"context"
	"log/slog"
	"testing"
	"time"
)

// fakeSlot is a kernel-table stand-in: a single mutable owner.
type fakeSlot struct {
	owner    string
	activate int
}

func (f *fakeSlot) Read(context.Context) (string, error) { return f.owner, nil }
func (f *fakeSlot) Clear(context.Context) error          { f.owner = ""; return nil }
func (f *fakeSlot) Describe() string                     { return "fake slot" }
func (f *fakeSlot) Activate(_ context.Context, n string) error {
	f.owner = n
	f.activate++
	return nil
}

type engineHarness struct {
	e    *Engine
	slot *fakeSlot
	now  time.Time
	ctx  context.Context
}

func newHarness(slotOwner string, failMode FailMode) *engineHarness {
	h := &engineHarness{slot: &fakeSlot{owner: slotOwner}, now: t0, ctx: context.Background()}
	cfg := EngineConfig{
		Name:   "freedom",
		Roster: roster(),
		Health: HealthConfig{UpThreshold: 1, DownThreshold: 3, MetricWindow: 10},
		Window: 30 * time.Minute, SeedBudget: 30 * time.Second,
		FailMode: failMode, LossTol: 0.05, ReconcileEvery: 2 * time.Second,
	}
	h.e = NewEngine(cfg, h.slot, nil, slog.Default(), func() time.Time { return h.now })
	return h
}

// step advances time, feeds one probe round, and reconciles — one
// engine cycle without the goroutine loop.
func (h *engineHarness) step(t *testing.T, up map[string]bool) {
	t.Helper()
	h.now = h.now.Add(2 * time.Second)
	for name, ok := range up {
		h.e.observe(ProbeResult{Cand: name, OK: ok, RTT: 40 * time.Millisecond, At: h.now})
	}
	h.e.Reconcile(h.ctx)
}

func TestEngineAdoptsAndHolds(t *testing.T) {
	h := newHarness("wgse2", FailOpen)
	// a few healthy rounds: adopted current, cold timers, no switching
	for range 3 {
		h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": false})
	}
	if h.slot.owner != "wgse2" || h.slot.activate != 0 {
		t.Fatalf("restart must adopt, not switch: %+v", h.slot)
	}
	st := h.e.Snapshot()
	if st.Current != "wgse2" || st.Opaque {
		t.Fatalf("snapshot current: %+v", st)
	}
}

func TestEngineUpgradeAfterSoak(t *testing.T) {
	h := newHarness("wgse2", FailOpen)
	// wgse alive throughout; cold-start timers mean the soak counts
	// from the daemon's first verdict
	for range 3 {
		h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	}
	if h.slot.owner != "wgse2" {
		t.Fatal("must hold during soak")
	}
	h.now = h.now.Add(31 * time.Minute) // clock jumps past the window
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	if h.slot.owner != "wgse" {
		t.Fatalf("soaked preferred must take the slot: %+v", h.slot)
	}
}

func TestEngineInstantFailoverOnVerdict(t *testing.T) {
	h := newHarness("wgse", FailOpen)
	for range 2 {
		h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": false})
	}
	// current dies: 3 misses → verdict flips → reconcile fires from
	// observe, not from the next tick
	for range 3 {
		h.step(t, map[string]bool{"wgse": false, "wgse2": true, "wgie": false})
	}
	if h.slot.owner != "wgse2" {
		t.Fatalf("failover to the live fallback: %+v", h.slot)
	}
	// wgse comes back: equal treatment would move back instantly, but
	// upgrade needs the full window again
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": false})
	if h.slot.owner != "wgse2" {
		t.Fatalf("no instant return to the preferred: %+v", h.slot)
	}
}

func TestEngineFailOpenThenReseed(t *testing.T) {
	h := newHarness("wgse", FailOpen)
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	// everything dies → all proven dead → slot cleared
	for range 3 {
		h.step(t, map[string]bool{"wgse": false, "wgse2": false, "wgie": false})
	}
	if h.slot.owner != "" {
		t.Fatalf("fail-open must clear the slot: %+v", h.slot)
	}
	// a junior recovers; the seniors are PROVEN dead (probed all along),
	// so there is nothing to defer for — reseed immediately
	h.step(t, map[string]bool{"wgse": false, "wgse2": true, "wgie": false})
	if h.slot.owner != "wgse2" {
		t.Fatalf("junior must reseed at once past proven-dead seniors: %+v", h.slot)
	}
	// the senior then recovers → back to soak-gated upgrade, no jump
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": false})
	if h.slot.owner != "wgse2" {
		t.Fatalf("recovered senior must soak before preempting: %+v", h.slot)
	}
}

func TestEngineSeedBudgetExpiry(t *testing.T) {
	h := newHarness("", FailOpen)
	h.step(t, map[string]bool{"wgse": false, "wgse2": true, "wgie": true})
	if h.slot.owner != "" {
		t.Fatal("junior seeds must defer within the budget")
	}
	h.now = h.now.Add(31 * time.Second)
	h.step(t, map[string]bool{"wgse": false, "wgse2": true, "wgie": true})
	if h.slot.owner != "wgse2" {
		t.Fatalf("budget expiry seeds best alive (subprio tiebreak): %+v", h.slot)
	}
}

func TestEngineOpaqueOwnerNeverTouched(t *testing.T) {
	h := newHarness("tailscale0", FailOpen)
	for range 5 {
		h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	}
	if h.slot.owner != "tailscale0" || h.slot.activate != 0 {
		t.Fatalf("opaque owner must be held: %+v", h.slot)
	}
	// admin removes it → empty slot → normal seed takes over
	h.slot.owner = ""
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	if h.slot.owner != "wgse" {
		t.Fatalf("freed slot must be seeded: %+v", h.slot)
	}
}

func TestEngineExternalChangeAdopted(t *testing.T) {
	h := newHarness("wgse", FailOpen)
	for range 2 {
		h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	}
	// admin moves the slot by hand to a roster member — adopted, held
	h.slot.owner = "wgie"
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	st := h.e.Snapshot()
	if st.Current != "wgie" {
		t.Fatalf("external change must be adopted: %+v", st)
	}
	if h.slot.activate != 0 {
		t.Fatalf("adopting must not activate (timers are cold): %+v", h.slot)
	}
}
