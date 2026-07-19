// Package core implements the transport-blind selector engine: the
// candidate policy order, per-candidate health tracking with asymmetric
// hysteresis, the pure decision function, and the reconcile loop that
// drives a slot through injected transport hooks.
package core

import "time"

// Rank orders candidates statically; higher values are preferred.
//
// Prio is the preemption axis: a candidate with a strictly higher Prio
// than the current one, once stably alive for the upgrade window,
// demands the slot. Subprio never preempts anything: it only breaks
// Prio ties at forced choice points (seed, failover).
type Rank struct {
	Prio    int
	Subprio int
}

// Compare returns >0 when r is preferred over o, <0 when o wins, 0 on a tie.
func (r Rank) Compare(o Rank) int {
	if r.Prio != o.Prio {
		return r.Prio - o.Prio
	}
	return r.Subprio - o.Subprio
}

// Metric is the measured tail of the candidate order, fed by the same
// probe stream as liveness: loss over the sample window compares first
// (within a tolerance), then mean RTT. Like Subprio, it never moves a
// working candidate; it only breaks full (Prio, Subprio) ties at forced
// choice points.
type Metric struct {
	Loss    float64       // lost fraction over the window; 1 when no samples yet
	RTT     time.Duration // mean RTT of successful probes; 0 means "no data"
	Samples int
}

// Compare returns >0 when m is better than o, <0 when worse, 0 when
// equal: losses within tol are a tie that falls through to RTT, where
// lower wins and "no data" (zero) loses to any measurement.
//
// The tolerance makes ties non-transitive in general; callers resolve
// the ambiguity deterministically by roster order.
func (m Metric) Compare(o Metric, tol float64) int {
	switch d := m.Loss - o.Loss; {
	case d < -tol:
		return 1
	case d > tol:
		return -1
	}
	switch {
	case m.RTT == o.RTT:
		return 0
	case m.RTT == 0:
		return -1
	case o.RTT == 0:
		return 1
	case m.RTT < o.RTT:
		return 1
	default:
		return -1
	}
}

// Candidate is a roster entry. Roster order is the final deterministic
// tiebreak: on a full (Rank, Metric) tie the earlier entry wins.
type Candidate struct {
	Name string
	Rank Rank
}

// Status is a candidate's debounced probe verdict. A candidate starts
// Unknown and acquires a verdict only from enough consecutive evidence;
// the engine takes no slot action that depends on a candidate while it
// is Unknown ("no switching without data").
type Status int

const (
	Unknown Status = iota
	Alive
	Dead
)

func (s Status) String() string {
	switch s {
	case Alive:
		return "alive"
	case Dead:
		return "dead"
	default:
		return "unknown"
	}
}

// CandState is one candidate's view at a decision instant.
type CandState struct {
	Status     Status
	AliveSince time.Time // entry into debounced-Alive; may predate the daemon (state file)
	Metric     Metric

	// FailStreak and LastAt carry the freshest raw evidence beneath the
	// debounced verdict: consecutive lost probes ending at the last
	// result's completion time. Forced choices consult them because a
	// verdict can be stale against the probe stream — an Alive candidate
	// on an active fail streak is mid-collapse, a Dead one from a shared
	// outage may be fine again (see Clean and FreshlyDead).
	FailStreak int
	LastAt     time.Time
}

// Clean reports a candidate fit to receive the slot at a forced choice:
// debounced-Alive and its latest probe succeeded. Alive on an active
// fail streak is suspect — likely mid-transition to Dead during a
// shared outage — and switching onto it buys nothing.
func (s CandState) Clean() bool { return s.Status == Alive && s.FailStreak == 0 }

// FreshlyDead reports death proven by evidence at least as new as
// since: debounced-Dead with the latest probe — completed at or after
// since — lost. A bare Dead verdict can be storm-era news (the shared
// outage that produced it may be over), and a Dead candidate whose
// recent probes succeed is recovering, not freshly dead.
func (s CandState) FreshlyDead(since time.Time) bool {
	return s.Status == Dead && s.FailStreak > 0 && !s.LastAt.Before(since)
}
