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
}
