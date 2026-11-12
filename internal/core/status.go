package core

import (
	"fmt"
	"time"
)

// SelectorStatus is one selector's full picture for the control socket:
// enough to answer "why has it not switched?" without reading code.
type SelectorStatus struct {
	Name          string            `json:"name"`
	Slot          string            `json:"slot"`
	FailMode      string            `json:"fail_mode"`
	Window        string            `json:"upgrade_window"`
	Current       string            `json:"current"`
	Opaque        bool              `json:"opaque,omitempty"`
	CurrentSince  time.Time         `json:"current_since,omitzero"`
	ForcedAt      time.Time         `json:"forced_at,omitzero"`
	LastDecision  string            `json:"last_decision"`
	SeedDeadline  time.Time         `json:"seed_deadline,omitzero"`
	GraceDeadline time.Time         `json:"grace_deadline,omitzero"`
	SlotError     string            `json:"slot_error,omitempty"`
	Candidates    []CandidateStatus `json:"candidates"`
}

type CandidateStatus struct {
	Name        string    `json:"name"`
	Prio        int       `json:"prio"`
	Subprio     int       `json:"subprio"`
	Status      string    `json:"status"`
	StatusSince time.Time `json:"status_since,omitzero"`
	AliveSince  time.Time `json:"alive_since,omitzero"`
	Loss        float64   `json:"loss"`
	RTTMs       float64   `json:"rtt_ms"`
	Samples     int       `json:"samples"`
	FailStreak  int       `json:"fail_streak,omitempty"`
	LastError   string    `json:"last_error,omitempty"`
	Verdict     string    `json:"verdict"`
}

func failModeString(m FailMode) string {
	if m == FailClosed {
		return "closed"
	}
	return "open"
}

// Snapshot renders the engine state for the control socket.
func (e *Engine) Snapshot() SelectorStatus {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()

	st := SelectorStatus{
		Name:         e.cfg.Name,
		Slot:         e.slot.Describe(),
		FailMode:     failModeString(e.cfg.FailMode),
		Window:       e.cfg.Window.String(),
		Current:      e.current,
		Opaque:       e.opaque,
		CurrentSince: e.currentSince,
		ForcedAt:     e.forceAt,
		LastDecision: e.lastDecision,
		SlotError:    e.lastSlotErr,
	}
	if e.slotEmpty && now.Before(e.seedDeadline) {
		st.SeedDeadline = e.seedDeadline
	}
	if now.Before(e.graceDeadline) {
		st.GraceDeadline = e.graceDeadline
	}

	curRank, curAlive := Rank{}, false
	if !e.opaque && e.current != "" {
		curRank = e.rankOf(e.current)
		curAlive = e.trackers[e.current].State().Status == Alive
	}

	for _, c := range e.cfg.Roster {
		t := e.trackers[c.Name]
		s := t.State()
		cs := CandidateStatus{
			Name:        c.Name,
			Prio:        c.Rank.Prio,
			Subprio:     c.Rank.Subprio,
			Status:      s.Status.String(),
			StatusSince: t.statusSince,
			AliveSince:  s.AliveSince,
			Loss:        s.Metric.Loss,
			RTTMs:       float64(s.Metric.RTT.Microseconds()) / 1000,
			Samples:     s.Metric.Samples,
			FailStreak:  t.failStreak,
			LastError:   t.lastErr,
		}
		cs.Verdict = e.verdict(c, t, s, curRank, curAlive, now)
		st.Candidates = append(st.Candidates, cs)
	}
	return st
}

func (e *Engine) rankOf(name string) Rank {
	for _, c := range e.cfg.Roster {
		if c.Name == name {
			return c.Rank
		}
	}
	return Rank{}
}

// verdict is the one-line human answer for a candidate's row.
func (e *Engine) verdict(c Candidate, tr *Tracker, s CandState, curRank Rank, curAlive bool, now time.Time) string {
	if c.Name == e.current {
		return "current"
	}
	switch s.Status {
	case Unknown:
		return "no verdict yet (warming up)"
	case Dead:
		msg := fmt.Sprintf("dead since %s (%d probes lost)", tr.statusSince.Format(time.TimeOnly), tr.failStreak)
		if tr.lastErr != "" {
			msg += ": " + tr.lastErr
		}
		return msg
	}
	aliveFor := now.Sub(s.AliveSince).Round(time.Second)
	if e.current == "" || e.opaque || !curAlive {
		if s.FailStreak > 0 {
			return fmt.Sprintf("alive %s, losing (%d probes) — not a failover target", aliveFor, s.FailStreak)
		}
		return fmt.Sprintf("alive %s", aliveFor)
	}
	if c.Rank.Prio <= curRank.Prio {
		return fmt.Sprintf("alive %s; never preempts a working current (prio %d ≤ %d)",
			aliveFor, c.Rank.Prio, curRank.Prio)
	}
	// a recent admin force restarts the challenger soak (Input.ForceAt)
	if e.forceAt.After(s.AliveSince) {
		if left := e.cfg.Window - now.Sub(e.forceAt); left > 0 {
			return fmt.Sprintf("alive %s — soak restarted by admin force at %s; preempts current in %s",
				aliveFor, e.forceAt.Format(time.TimeOnly), left.Round(time.Second))
		}
	}
	if left := e.cfg.Window - now.Sub(s.AliveSince); left > 0 {
		return fmt.Sprintf("alive %s of %s window; preempts current in %s",
			aliveFor, e.cfg.Window, left.Round(time.Second))
	}
	return "eligible to preempt current"
}
