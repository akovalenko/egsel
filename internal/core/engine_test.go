package core

import (
	"context"
	"log/slog"
	"path/filepath"
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
	return newHarnessWithStore(slotOwner, failMode, nil)
}

func newHarnessWithStore(slotOwner string, failMode FailMode, store *Store) *engineHarness {
	h := &engineHarness{slot: &fakeSlot{owner: slotOwner}, now: t0, ctx: context.Background()}
	cfg := EngineConfig{
		Name:   "freedom",
		Roster: roster(),
		Health: HealthConfig{UpThreshold: 1, DownThreshold: 3, MetricWindow: 10},
		Window: 30 * time.Minute, SeedBudget: 30 * time.Second,
		FailMode: failMode, LossTol: 0.05, ReconcileEvery: 2 * time.Second,
	}
	h.e = NewEngine(cfg, h.slot, store, slog.Default(), func() time.Time { return h.now })
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

// graceHarness is newHarness with the fresh-evidence grace armed, as
// the daemon configures it (derived from probe timings).
func graceHarness(slotOwner string, failMode FailMode) *engineHarness {
	h := newHarness(slotOwner, failMode)
	h.e.cfg.Grace = 5 * time.Second
	return h
}

func TestEngineSharedOutageBlipNeverSwitches(t *testing.T) {
	h := graceHarness("wgse", FailOpen)
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	// provider outage: everyone starts losing; the current (preferred)
	// phase-races to its Dead verdict first
	for range 2 {
		h.step(t, map[string]bool{"wgse": false, "wgse2": false, "wgie": false})
	}
	h.step(t, map[string]bool{"wgse": false}) // wgse's 3rd miss → Dead
	if h.slot.owner != "wgse" || h.slot.activate != 0 {
		t.Fatalf("must not hop onto a losing fallback: %+v", h.slot)
	}
	// the blip ends; a fallback's probe cleans up first — the current's
	// death is storm-era news, it gets a fresh probe round
	h.step(t, map[string]bool{"wgse2": true})
	if h.slot.owner != "wgse" || h.slot.activate != 0 {
		t.Fatalf("stale-dead preferred must get its fresh probe: %+v", h.slot)
	}
	// ...and that probe succeeds: back to normal, zero switches made
	h.step(t, map[string]bool{"wgse": true, "wgie": true})
	if h.slot.owner != "wgse" || h.slot.activate != 0 {
		t.Fatalf("a blip must not move the slot at all: %+v", h.slot)
	}
}

func TestEngineRecoverySeedWaitsOutTheLottery(t *testing.T) {
	h := graceHarness("wgse", FailOpen)
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	// outage long enough to prove everyone dead → fail-open clears
	for range 3 {
		h.step(t, map[string]bool{"wgse": false, "wgse2": false, "wgie": false})
	}
	if h.slot.owner != "" {
		t.Fatalf("fail-open must clear the slot: %+v", h.slot)
	}
	// provider returns; the junior's probe happens to tick first — it
	// must not win the slot by phase alone
	h.step(t, map[string]bool{"wgse2": true})
	if h.slot.owner != "" {
		t.Fatalf("junior must not seed past a stale-dead senior: %+v", h.slot)
	}
	// the senior's probe ticks next: alive → seeded by rank, no soak
	h.step(t, map[string]bool{"wgse": true, "wgie": true})
	if h.slot.owner != "wgse" {
		t.Fatalf("recovered preferred must take the empty slot: %+v", h.slot)
	}
}

func TestEngineRecoverySeedYieldsOnFreshDeath(t *testing.T) {
	h := graceHarness("wgse", FailOpen)
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	for range 3 {
		h.step(t, map[string]bool{"wgse": false, "wgse2": false, "wgie": false})
	}
	// only the fallbacks came back: the senior's fresh probe is lost →
	// its death is confirmed, the junior seeds legitimately
	h.step(t, map[string]bool{"wgse2": true})
	h.step(t, map[string]bool{"wgse": false, "wgie": true})
	if h.slot.owner != "wgse2" {
		t.Fatalf("freshly-confirmed-dead senior must not block: %+v", h.slot)
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

func TestEngineForceSwitchesNowAndPolicyResumes(t *testing.T) {
	h := newHarness("wgse", FailOpen)
	// steady state: everyone healthy, the preferred soaked on the slot
	for range 2 {
		h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	}
	h.now = h.now.Add(time.Hour)
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})

	// the kick: switch right now, no rank/soak/freshness questions asked
	if err := h.e.Force(h.ctx, "wgse2"); err != nil {
		t.Fatal(err)
	}
	if h.slot.owner != "wgse2" {
		t.Fatalf("force must switch at once: %+v", h.slot)
	}
	st := h.e.Snapshot()
	if st.Current != "wgse2" || st.ForcedAt.IsZero() {
		t.Fatalf("snapshot after force: %+v", st)
	}

	// automatic policy resumes off the new state: the soaked senior does
	// not reclaim at once (fresh trust episode)...
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	if h.slot.owner != "wgse2" {
		t.Fatalf("forced current must not be reclaimed instantly: %+v", h.slot)
	}
	// ...but a full window past the kick it does
	h.now = h.now.Add(31 * time.Minute)
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	if h.slot.owner != "wgse" {
		t.Fatalf("re-soaked senior must reclaim the slot: %+v", h.slot)
	}
}

func TestEngineForceOntoDeadTriesAndFailsOver(t *testing.T) {
	h := newHarness("wgse", FailOpen)
	// wgse2 long dead, the current healthy
	for range 3 {
		h.step(t, map[string]bool{"wgse": true, "wgse2": false, "wgie": false})
	}
	// force onto the dead candidate anyway — the command is the evidence
	if err := h.e.Force(h.ctx, "wgse2"); err != nil {
		t.Fatal(err)
	}
	if h.slot.owner != "wgse2" {
		t.Fatalf("force must place even a dead target: %+v", h.slot)
	}
	// the next pass speaks: failover off the dead forced current
	h.step(t, map[string]bool{"wgse": true, "wgse2": false, "wgie": false})
	if h.slot.owner != "wgse" {
		t.Fatalf("automatic policy must fail over off the dead target: %+v", h.slot)
	}
}

func TestEngineForceUnknownCandidateRejected(t *testing.T) {
	h := newHarness("wgse", FailOpen)
	h.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	if err := h.e.Force(h.ctx, "nope"); err == nil {
		t.Fatal("unknown candidate must be rejected")
	}
	if h.slot.owner != "wgse" {
		t.Fatalf("rejected force must not touch the slot: %+v", h.slot)
	}
}

func TestEngineForceMarkSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	store := OpenStore(path, time.Minute, t0, slog.Default())
	h1 := newHarnessWithStore("wgse2", FailOpen, store)
	// healthy episode: wgse soaked (healthy-since persisted), current wgse2
	for range 2 {
		h1.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	}
	h1.now = h1.now.Add(time.Hour)
	h1.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	if err := h1.e.Force(h1.ctx, "wgse2"); err != nil {
		t.Fatal(err)
	}

	// a restart reloads the file (fresh store, adopted slot) and must
	// keep the episode boundary: without it, the restored healthy-since
	// makes the soaked senior preempt on the very first pass
	restartAt := t0.Add(time.Hour).Add(10 * time.Second)
	h2 := newHarnessWithStore("wgse2", FailOpen, OpenStore(path, time.Minute, restartAt, slog.Default()))
	h2.now = restartAt
	h2.step(t, map[string]bool{"wgse": true, "wgse2": true, "wgie": true})
	if h2.slot.owner != "wgse2" {
		t.Fatalf("restart must keep the force episode: %+v", h2.slot)
	}
	if h2.e.Snapshot().ForcedAt.IsZero() {
		t.Fatal("force mark must be restored into the snapshot")
	}
}
