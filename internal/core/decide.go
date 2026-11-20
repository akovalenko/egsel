package core

import (
	"fmt"
	"time"
)

// FailMode says what to do with the slot when no candidate is reachable.
type FailMode int

const (
	// FailOpen clears the slot: traffic falls through to whatever
	// routing remains. Right when the slot is an *extra* path and its
	// absence has a sane fallback (murphy: no default in table freedom
	// means clients leave via the home uplink).
	FailOpen FailMode = iota
	// FailClosed keeps the slot pointed at the statically best candidate
	// even if it is dead: a dead route beats no route when the slot is
	// the machine's only default (the table = "main" multi-WAN case).
	FailClosed
)

// Op is what Decide wants done to the slot.
type Op int

const (
	Hold Op = iota
	Activate
	Clear
)

// Input is the full picture at one decision instant; Decide is pure.
type Input struct {
	// Current names the slot's present owner: a roster name, "" for an
	// empty slot, or (with Opaque) a device outside the roster.
	Current string
	Opaque  bool

	// ForceAt is the moment of the last admin force ("egsel force"), the
	// boundary of a fresh trust episode for whoever holds the slot:
	// challengers count their upgrade soak from this moment, not from
	// their own (possibly much older) alive-since, so a forced current is
	// not instantly reclaimed by an already-soaked senior. Everything
	// else — failover, seed, fail modes, freshness demands — runs
	// untouched: the force switches now; the policy speaks after. Zero
	// when no force happened.
	ForceAt time.Time

	Roster []Candidate
	States map[string]CandState

	// Window is the cross-prio upgrade soak: a higher-prio candidate
	// preempts a working current only after being continuously Alive
	// this long.
	Window time.Duration
	// SeedDeadline gates an empty slot: before it, only a candidate of
	// the top static rank may seed (juniors wait for seniors to warm
	// up); after it, the best reachable one wins.
	SeedDeadline time.Time
	// FreshSince anchors evidence freshness at forced choices: a junior
	// may take the slot past a higher-prio candidate only on death
	// evidence at least this new (see CandState.FreshlyDead). The engine
	// re-arms it whenever a clean candidate appears after a spell with
	// none — the signature of a shared outage ending, when storm-era
	// verdicts must not be trusted.
	FreshSince time.Time
	// GraceDeadline bounds the fresh-evidence wait — roughly one probe
	// round per senior; past it, stale verdicts are acted on anyway (a
	// wedged prober must not pin the slot). Zero disables the wait.
	GraceDeadline time.Time
	FailMode      FailMode
	LossTol       float64

	Now time.Time
}

type Decision struct {
	Op     Op
	Target string // Activate only
	Reason string
}

// Decide picks the desired slot action. The asymmetric hysteresis lives
// in the case split: failover off a dead current is immediate and
// ignores the window; an upgrade away from a *working* current needs a
// strictly higher Prio held alive for the whole window; equal-prio
// candidates never displace a working current at all. Forced choices
// (failover, seed) additionally demand fresh evidence — clean targets,
// freshly-proven-dead seniors — so that a shared outage killing every
// candidate at once neither hops onto a dying fallback nor hands the
// recovered slot to whichever junior's probe ticked first. An admin
// force bypasses none of this: it switches the slot directly (see
// Engine.Force) and only moves the soak baseline (ForceAt).
func Decide(in Input) Decision {
	if in.Opaque {
		return Decision{Hold, "", fmt.Sprintf("slot owned by non-roster %q; holding (cannot assess it)", in.Current)}
	}
	if in.Current == "" {
		return decideSeed(in)
	}
	cur := in.States[in.Current]
	switch cur.Status {
	case Alive:
		return decideUpgrade(in, cur)
	case Dead:
		return decideFailover(in)
	default:
		return Decision{Hold, "", fmt.Sprintf("no probe verdict on current %s yet; holding", in.Current)}
	}
}

func decideSeed(in Input) Decision {
	best, ok := bestForced(in, CandState.Clean)
	if !ok {
		if in.FailMode == FailClosed && in.allProvenDead() {
			c, _ := bestForced(in, func(CandState) bool { return true })
			return Decision{Activate, c.Name,
				fmt.Sprintf("no candidate reachable; fail-closed seeds best-ranked %s (a route beats none)", c.Name)}
		}
		return Decision{Hold, "", "slot empty, no reachable candidate"}
	}
	top := topStaticRank(in.Roster)
	if best.Rank.Compare(top) == 0 {
		return Decision{Activate, best.Name, fmt.Sprintf("seed: top-ranked %s reachable", best.Name)}
	}
	// Defer for a higher-ranked candidate while one is still unproven
	// (cold-boot warm-up race) ...
	if in.Now.Before(in.SeedDeadline) && in.anyHigherUnproven(best.Rank) {
		return Decision{Hold, "", fmt.Sprintf("seed: %s reachable but deferring for higher-ranked until %s",
			best.Name, in.SeedDeadline.Format(time.DateTime))}
	}
	// ... or proven Dead only by storm-era evidence (the post-outage
	// seed is otherwise a probe-phase lottery): within the grace, only a
	// freshly lost probe writes a senior off.
	if name, held := in.seniorLacksFreshVerdict(best); held {
		return Decision{Hold, "", fmt.Sprintf("seed: %s reachable, but higher-prio %s has no fresh verdict — holding until %s",
			best.Name, name, in.GraceDeadline.Format(time.DateTime))}
	}
	return Decision{Activate, best.Name, fmt.Sprintf("seed: best reachable is %s", best.Name)}
}

// anyHigherUnproven reports whether some candidate ranked strictly
// above r still lacks a Dead verdict (it is Unknown; were it Alive it
// would have been the best pick itself).
func (in Input) anyHigherUnproven(r Rank) bool {
	for _, c := range in.Roster {
		if c.Rank.Compare(r) > 0 && in.States[c.Name].Status != Dead {
			return true
		}
	}
	return false
}

// seniorLacksFreshVerdict names a candidate with Prio strictly above
// target's whose death is not proven by fresh evidence (FreshlyDead)
// while the grace deadline runs. Activating target past such a senior
// is the post-outage lottery: seconds later the senior proves alive and
// sits out a whole upgrade window. Alive-but-suspect and recovering
// (Dead, recent probes succeeding) seniors block too; each resolves
// within about one probe round — alive seniors become the target
// themselves, dead ones refresh their proof with the next lost probe.
func (in Input) seniorLacksFreshVerdict(target Candidate) (string, bool) {
	if !in.Now.Before(in.GraceDeadline) {
		return "", false
	}
	for _, c := range in.Roster {
		if c.Rank.Prio > target.Rank.Prio && !in.States[c.Name].FreshlyDead(in.FreshSince) {
			return c.Name, true
		}
	}
	return "", false
}

func decideUpgrade(in Input, cur CandState) Decision {
	curRank := in.rank(in.Current)
	var eligible []Candidate
	var pending *Candidate // best higher-prio alive one still soaking, for the reason
	var pendingFor time.Duration
	for i, c := range in.Roster {
		if c.Rank.Prio <= curRank.Prio {
			continue
		}
		s := in.States[c.Name]
		if s.Status != Alive {
			continue
		}
		if aliveFor := in.Now.Sub(in.soakStart(s)); aliveFor >= in.Window {
			eligible = append(eligible, c)
		} else if pending == nil || aliveFor > pendingFor {
			pending = &in.Roster[i]
			pendingFor = aliveFor
		}
	}
	if len(eligible) > 0 {
		t := pickBest(eligible, in.States, in.LossTol)
		return Decision{Activate, t.Name,
			fmt.Sprintf("upgrade: %s continuously alive %s ≥ window %s", t.Name,
				in.Now.Sub(in.soakStart(in.States[t.Name])).Round(time.Second), in.Window)}
	}
	if pending != nil {
		return Decision{Hold, "", fmt.Sprintf("current %s alive; %s alive %s of %s window — not yet eligible to preempt",
			in.Current, pending.Name, pendingFor.Round(time.Second), in.Window)}
	}
	return Decision{Hold, "", fmt.Sprintf("current %s alive; no higher-prio candidate alive", in.Current)}
}

// soakStart is when a challenger's continuous liveness began counting
// toward preempting the current: its own alive-since, or the last admin
// force — whichever is later. A forced current opens a fresh trust
// episode; soak evidence older than the kick no longer evicts it.
func (in Input) soakStart(s CandState) time.Time {
	if in.ForceAt.After(s.AliveSince) {
		return in.ForceAt
	}
	return s.AliveSince
}

func decideFailover(in Input) Decision {
	best, ok := bestForced(in, CandState.Clean)
	if ok {
		if name, held := in.seniorLacksFreshVerdict(best); held {
			return Decision{Hold, "", fmt.Sprintf(
				"current %s dead; %s reachable, but higher-prio %s has no fresh verdict — holding until %s",
				in.Current, best.Name, name, in.GraceDeadline.Format(time.DateTime))}
		}
		return Decision{Activate, best.Name,
			fmt.Sprintf("failover: current %s dead, best reachable is %s", in.Current, best.Name)}
	}
	if !in.allProvenDead() {
		return Decision{Hold, "", fmt.Sprintf("current %s dead; waiting for clean verdicts on remaining candidates", in.Current)}
	}
	if in.FailMode == FailOpen {
		return Decision{Clear, "", "no candidate reachable; failing open (clearing slot)"}
	}
	c, _ := bestForced(in, func(CandState) bool { return true })
	if c.Name == in.Current {
		return Decision{Hold, "", "no candidate reachable; fail-closed keeps best-ranked current"}
	}
	return Decision{Activate, c.Name,
		fmt.Sprintf("no candidate reachable; fail-closed points slot at best-ranked %s", c.Name)}
}

func (in Input) rank(name string) Rank {
	for _, c := range in.Roster {
		if c.Name == name {
			return c.Rank
		}
	}
	return Rank{}
}

func (in Input) allProvenDead() bool {
	for _, c := range in.Roster {
		if in.States[c.Name].Status != Dead {
			return false
		}
	}
	return true
}

func topStaticRank(roster []Candidate) Rank {
	top := roster[0].Rank
	for _, c := range roster[1:] {
		if c.Rank.Compare(top) > 0 {
			top = c.Rank
		}
	}
	return top
}

// bestForced returns the candidate preferred at a forced choice point —
// the maximum by (Prio, Subprio, Metric) with roster order as the final
// tiebreak — among candidates whose state passes keep.
func bestForced(in Input, keep func(CandState) bool) (Candidate, bool) {
	var kept []Candidate
	for _, c := range in.Roster {
		if keep(in.States[c.Name]) {
			kept = append(kept, c)
		}
	}
	if len(kept) == 0 {
		return Candidate{}, false
	}
	return pickBest(kept, in.States, in.LossTol), true
}

func pickBest(cands []Candidate, states map[string]CandState, tol float64) Candidate {
	best := cands[0]
	for _, c := range cands[1:] {
		if r := c.Rank.Compare(best.Rank); r > 0 {
			best = c
		} else if r == 0 && states[c.Name].Metric.Compare(states[best.Name].Metric, tol) > 0 {
			best = c
		}
	}
	return best
}
