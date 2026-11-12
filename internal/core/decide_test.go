package core

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)

// murphy-shaped roster: one preferred exit, two equal-prio fallbacks
// with a subprio lean.
func roster() []Candidate {
	return []Candidate{
		{Name: "wgse", Rank: Rank{Prio: 100}},
		{Name: "wgse2", Rank: Rank{Prio: 50, Subprio: 10}},
		{Name: "wgie", Rank: Rank{Prio: 50}},
	}
}

func alive(since time.Time) CandState {
	return CandState{Status: Alive, AliveSince: since, Metric: Metric{Loss: 0, RTT: 50 * time.Millisecond, Samples: 30}}
}

func dead() CandState { return CandState{Status: Dead, Metric: Metric{Loss: 1, Samples: 30}} }

func unknown() CandState { return CandState{Metric: Metric{Loss: 1}} }

// suspect is Alive whose latest probes are being lost: the verdict is
// stale against the stream (mid-collapse of a shared outage).
func suspect(since time.Time) CandState {
	s := alive(since)
	s.FailStreak = 2
	s.LastAt = t0
	return s
}

// deadAt is Dead with its losing streak's last probe completed at last:
// fresh or stale depends on the caller's FreshSince.
func deadAt(last time.Time) CandState {
	s := dead()
	s.FailStreak = 3
	s.LastAt = last
	return s
}

func input(current string, states map[string]CandState) Input {
	return Input{
		Current: current,
		Roster:  roster(),
		States:  states,
		Window:  30 * time.Minute,
		LossTol: 0.05,
		Now:     t0,
	}
}

func TestUpgradeOnlyByStrictlyHigherPrioAfterWindow(t *testing.T) {
	// sitting on a fallback; preferred alive but young → hold
	in := input("wgie", map[string]CandState{
		"wgse":  alive(t0.Add(-10 * time.Minute)),
		"wgse2": alive(t0.Add(-2 * time.Hour)),
		"wgie":  alive(t0.Add(-2 * time.Hour)),
	})
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("young preferred must not preempt: %+v", d)
	}
	// window matured → upgrade to wgse
	in.States["wgse"] = alive(t0.Add(-31 * time.Minute))
	if d := Decide(in); d.Op != Activate || d.Target != "wgse" {
		t.Fatalf("matured preferred must preempt: %+v", d)
	}
}

func TestEqualPrioNeverPreempts(t *testing.T) {
	// on wgie; wgse2 (same prio, higher subprio) alive for ages; wgse dead
	in := input("wgie", map[string]CandState{
		"wgse":  dead(),
		"wgse2": alive(t0.Add(-24 * time.Hour)),
		"wgie":  alive(t0.Add(-24 * time.Hour)),
	})
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("equal prio must never preempt a working current: %+v", d)
	}
}

func TestFailoverPicksBestAliveIgnoringWindow(t *testing.T) {
	// current dead; preferred alive only briefly — failover may climb up
	// in prio anyway (variant (a)): window guards only upgrades.
	in := input("wgie", map[string]CandState{
		"wgse":  alive(t0.Add(-10 * time.Second)),
		"wgse2": alive(t0.Add(-1 * time.Hour)),
		"wgie":  dead(),
	})
	if d := Decide(in); d.Op != Activate || d.Target != "wgse" {
		t.Fatalf("failover should take best alive incl. higher prio: %+v", d)
	}
}

func TestFailoverSubprioTiebreak(t *testing.T) {
	in := input("wgse", map[string]CandState{
		"wgse":  dead(),
		"wgse2": alive(t0.Add(-1 * time.Hour)),
		"wgie":  alive(t0.Add(-1 * time.Hour)),
	})
	if d := Decide(in); d.Op != Activate || d.Target != "wgse2" {
		t.Fatalf("subprio should break the fallback tie: %+v", d)
	}
}

func TestFailoverMetricTiebreak(t *testing.T) {
	in := input("wgse", map[string]CandState{
		"wgse": dead(),
		"wgse2": {Status: Alive, AliveSince: t0.Add(-time.Hour),
			Metric: Metric{Loss: 0.3, RTT: 50 * time.Millisecond, Samples: 30}},
		"wgie": {Status: Alive, AliveSince: t0.Add(-time.Hour),
			Metric: Metric{Loss: 0, RTT: 90 * time.Millisecond, Samples: 30}},
	})
	// equal (prio); wgse2 wins subprio but loses metric... subprio is
	// senior to metric, so wgse2 still wins.
	if d := Decide(in); d.Target != "wgse2" {
		t.Fatalf("subprio outranks metric: %+v", d)
	}
	// strip the subprio lean → metric decides: wgie has less loss
	in.Roster[1].Rank.Subprio = 0
	if d := Decide(in); d.Target != "wgie" {
		t.Fatalf("metric should break a full static tie: %+v", d)
	}
}

func TestMetricRTTBreaksLossTie(t *testing.T) {
	states := map[string]CandState{
		"wgse": dead(),
		"wgse2": {Status: Alive, AliveSince: t0.Add(-time.Hour),
			Metric: Metric{Loss: 0.03, RTT: 90 * time.Millisecond, Samples: 30}},
		"wgie": {Status: Alive, AliveSince: t0.Add(-time.Hour),
			Metric: Metric{Loss: 0, RTT: 40 * time.Millisecond, Samples: 30}},
	}
	in := input("wgse", states)
	in.Roster[1].Rank.Subprio = 0 // full static tie
	// losses within tol (0.03 ≤ 0.05) → RTT decides → wgie
	if d := Decide(in); d.Target != "wgie" {
		t.Fatalf("RTT should break in-tolerance loss tie: %+v", d)
	}
}

func TestFailoverWaitsForVerdictsBeforeFailAction(t *testing.T) {
	// current dead, another candidate still unknown → hold, do not clear
	in := input("wgse", map[string]CandState{
		"wgse":  dead(),
		"wgse2": unknown(),
		"wgie":  dead(),
	})
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("must wait for verdicts before failing open: %+v", d)
	}
	// all proven dead → fail-open clears
	in.States["wgse2"] = dead()
	if d := Decide(in); d.Op != Clear {
		t.Fatalf("fail-open should clear when all proven dead: %+v", d)
	}
}

func TestFailClosedPointsAtBestRankedDead(t *testing.T) {
	in := input("wgie", map[string]CandState{
		"wgse": dead(), "wgse2": dead(), "wgie": dead(),
	})
	in.FailMode = FailClosed
	if d := Decide(in); d.Op != Activate || d.Target != "wgse" {
		t.Fatalf("fail-closed should point at best static rank: %+v", d)
	}
	// already on the best-ranked one → hold
	in.Current = "wgse"
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("fail-closed on best-ranked current should hold: %+v", d)
	}
}

func TestSeedPrefersTopRankImmediately(t *testing.T) {
	in := input("", map[string]CandState{
		"wgse":  alive(t0.Add(-2 * time.Second)),
		"wgse2": alive(t0.Add(-10 * time.Second)),
		"wgie":  unknown(),
	})
	in.SeedDeadline = t0.Add(20 * time.Second)
	if d := Decide(in); d.Op != Activate || d.Target != "wgse" {
		t.Fatalf("top-ranked alive must seed immediately: %+v", d)
	}
}

func TestSeedDefersJuniorUntilDeadline(t *testing.T) {
	in := input("", map[string]CandState{
		"wgse":  unknown(),
		"wgse2": alive(t0.Add(-3 * time.Second)),
		"wgie":  unknown(),
	})
	in.SeedDeadline = t0.Add(10 * time.Second)
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("junior must wait for the senior within budget: %+v", d)
	}
	in.SeedDeadline = t0.Add(-time.Second)
	if d := Decide(in); d.Op != Activate || d.Target != "wgse2" {
		t.Fatalf("past the budget, seed takes the best alive: %+v", d)
	}
}

func TestSeedSkipsDeferralWhenSeniorsProvenDead(t *testing.T) {
	in := input("", map[string]CandState{
		"wgse":  dead(),
		"wgse2": alive(t0.Add(-3 * time.Second)),
		"wgie":  unknown(),
	})
	in.SeedDeadline = t0.Add(10 * time.Second)
	// nothing above wgse2 left to wait for → seed now, budget or not
	if d := Decide(in); d.Op != Activate || d.Target != "wgse2" {
		t.Fatalf("proven-dead seniors leave nothing to defer for: %+v", d)
	}
}

func TestSeedFailClosedWhenAllProvenDead(t *testing.T) {
	in := input("", map[string]CandState{
		"wgse": dead(), "wgse2": dead(), "wgie": dead(),
	})
	in.FailMode = FailClosed
	if d := Decide(in); d.Op != Activate || d.Target != "wgse" {
		t.Fatalf("fail-closed seeds best-ranked even when dead: %+v", d)
	}
	in.FailMode = FailOpen
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("fail-open leaves an empty slot empty: %+v", d)
	}
}

func TestOpaqueOwnerHeld(t *testing.T) {
	in := input("enp6s0", map[string]CandState{
		"wgse":  alive(t0.Add(-2 * time.Hour)),
		"wgse2": alive(t0.Add(-2 * time.Hour)),
		"wgie":  alive(t0.Add(-2 * time.Hour)),
	})
	in.Opaque = true
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("an opaque owner must never be evicted: %+v", d)
	}
}

func TestFailoverShunsSuspectTarget(t *testing.T) {
	// shared outage mid-collapse: current proven dead first, the
	// fallbacks' Alive verdicts lag their failing probes → hold, don't
	// hop onto a dying channel
	in := input("wgse", map[string]CandState{
		"wgse":  deadAt(t0),
		"wgse2": suspect(t0.Add(-2 * time.Hour)),
		"wgie":  suspect(t0.Add(-2 * time.Hour)),
	})
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("must not fail over onto a losing candidate: %+v", d)
	}
	// one fallback's probe succeeds → clean → normal instant failover
	// (the dead current's own streak is fresh evidence, no grace hold)
	in.States["wgse2"] = alive(t0.Add(-2 * time.Hour))
	in.FreshSince = t0.Add(-5 * time.Second)
	in.GraceDeadline = t0.Add(5 * time.Second)
	if d := Decide(in); d.Op != Activate || d.Target != "wgse2" {
		t.Fatalf("clean fallback with freshly-dead current must win: %+v", d)
	}
}

func TestFailoverHoldsForStaleDeadSenior(t *testing.T) {
	// blip just ended: a fallback is clean again, but the dead current's
	// (higher-prio) last lost probe predates the freshness anchor — its
	// death is storm-era news, give it a fresh probe round
	in := input("wgse", map[string]CandState{
		"wgse":  deadAt(t0.Add(-3 * time.Second)),
		"wgse2": alive(t0.Add(-2 * time.Hour)),
		"wgie":  deadAt(t0.Add(-3 * time.Second)),
	})
	in.FreshSince = t0.Add(-time.Second)
	in.GraceDeadline = t0.Add(5 * time.Second)
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("stale-dead senior must get a fresh probe round: %+v", d)
	}
	// the fresh probe is lost → death confirmed → failover proceeds
	in.States["wgse"] = deadAt(t0)
	if d := Decide(in); d.Op != Activate || d.Target != "wgse2" {
		t.Fatalf("freshly-confirmed death unblocks failover: %+v", d)
	}
}

func TestFailoverGraceExpiryActsOnStaleVerdicts(t *testing.T) {
	// a wedged prober must not pin the slot: past the grace deadline the
	// stale Dead is acted on anyway
	in := input("wgse", map[string]CandState{
		"wgse":  deadAt(t0.Add(-3 * time.Second)),
		"wgse2": alive(t0.Add(-2 * time.Hour)),
		"wgie":  deadAt(t0),
	})
	in.FreshSince = t0.Add(-time.Second)
	in.GraceDeadline = t0.Add(-time.Millisecond)
	if d := Decide(in); d.Op != Activate || d.Target != "wgse2" {
		t.Fatalf("expired grace must act on stale verdicts: %+v", d)
	}
}

func TestSeedHoldsForStaleDeadSenior(t *testing.T) {
	// post-outage reseed: the junior's probe ticked first, but the
	// seniors' Dead verdicts are storm-era — no probe-phase lottery
	in := input("", map[string]CandState{
		"wgse":  deadAt(t0.Add(-3 * time.Second)),
		"wgse2": alive(t0.Add(-time.Second)),
		"wgie":  deadAt(t0.Add(-3 * time.Second)),
	})
	in.SeedDeadline = t0.Add(-time.Minute) // long expired mid-outage
	in.FreshSince = t0.Add(-time.Second)
	in.GraceDeadline = t0.Add(5 * time.Second)
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("junior must not win the recovery by probe phase: %+v", d)
	}
	// the senior's fresh probe is lost → it really is dead → seed junior
	in.States["wgse"] = deadAt(t0)
	if d := Decide(in); d.Op != Activate || d.Target != "wgse2" {
		t.Fatalf("fresh death evidence lets the junior seed: %+v", d)
	}
}

func TestRecoveringSeniorBlocksJunior(t *testing.T) {
	// up_threshold > 1: the senior's recent probes succeed (okStreak
	// climbing) while still debounced-Dead — not freshly dead, wait
	recovering := dead()
	recovering.FailStreak = 0 // last probe succeeded
	recovering.LastAt = t0
	in := input("wgse2", map[string]CandState{
		"wgse":  recovering,
		"wgse2": deadAt(t0),
		"wgie":  alive(t0.Add(-2 * time.Hour)),
	})
	in.FreshSince = t0.Add(-5 * time.Second)
	in.GraceDeadline = t0.Add(5 * time.Second)
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("recovering senior must finish its climb first: %+v", d)
	}
}

func TestCurrentUnknownHolds(t *testing.T) {
	// daemon just started, adopted current, no probe verdicts yet
	in := input("wgse", map[string]CandState{
		"wgse": unknown(), "wgse2": unknown(), "wgie": unknown(),
	})
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("restart must not touch the slot before verdicts: %+v", d)
	}
}

func TestForceRestartsChallengerSoak(t *testing.T) {
	// a soaked senior normally reclaims the slot from a working junior
	in := input("wgse2", map[string]CandState{
		"wgse":  alive(t0.Add(-2 * time.Hour)),
		"wgse2": alive(t0.Add(-2 * time.Hour)),
		"wgie":  alive(t0.Add(-2 * time.Hour)),
	})
	if d := Decide(in); d.Op != Activate || d.Target != "wgse" {
		t.Fatalf("soaked senior must preempt: %+v", d)
	}
	// but the admin just forced the slot here: a fresh trust episode —
	// the challenger's soak counts from the kick, not its own clock
	in.ForceAt = t0.Add(-time.Minute)
	if d := Decide(in); d.Op != Hold {
		t.Fatalf("a forced current must not be reclaimed at once: %+v", d)
	}
	// a full window past the force, the senior preempts again
	in.ForceAt = t0.Add(-31 * time.Minute)
	if d := Decide(in); d.Op != Activate || d.Target != "wgse" {
		t.Fatalf("the re-soak matured — senior preempts: %+v", d)
	}
}
