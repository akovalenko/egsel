package core

import "time"

// HealthConfig sets the debounce thresholds and the metric window for
// one candidate's Tracker.
type HealthConfig struct {
	UpThreshold   int // consecutive successes to become Alive
	DownThreshold int // consecutive failures to become Dead
	MetricWindow  int // probes kept for the loss/RTT metric
}

type sample struct {
	ok  bool
	rtt time.Duration
}

// Tracker debounces one candidate's probe stream into a verdict and
// keeps the sliding loss/RTT window. The asymmetry the selector needs
// lives here: a single lost probe below DownThreshold does not reset
// the alive-since clock, while a real transition to Dead does.
type Tracker struct {
	cfg HealthConfig

	status      Status
	statusSince time.Time // last verdict change
	aliveSince  time.Time // continuous debounced-Alive; zero unless Alive
	okStreak    int
	failStreak  int

	// prior is a healthy-since restored from the state file. It is
	// adopted on a direct Unknown->Alive transition (continuity across a
	// short daemon restart is plausible) and dropped once the candidate
	// is proven Dead first (continuity broken).
	prior time.Time

	samples []sample

	lastAt  time.Time
	lastErr string
}

func NewTracker(cfg HealthConfig, prior time.Time) *Tracker {
	return &Tracker{cfg: cfg, prior: prior}
}

// Observe feeds one probe result and reports whether the verdict changed.
func (t *Tracker) Observe(r ProbeResult) bool {
	t.samples = append(t.samples, sample{ok: r.OK, rtt: r.RTT})
	if len(t.samples) > t.cfg.MetricWindow {
		t.samples = t.samples[1:]
	}
	t.lastAt = r.At
	t.lastErr = r.Err

	if r.OK {
		t.okStreak++
		t.failStreak = 0
	} else {
		t.failStreak++
		t.okStreak = 0
	}

	switch {
	case t.status != Alive && t.okStreak >= t.cfg.UpThreshold:
		was := t.status
		t.status = Alive
		t.statusSince = r.At
		t.aliveSince = r.At
		if was == Unknown && !t.prior.IsZero() {
			t.aliveSince = t.prior
		}
		t.prior = time.Time{}
		return true
	case t.status != Dead && t.failStreak >= t.cfg.DownThreshold:
		t.status = Dead
		t.statusSince = r.At
		t.aliveSince = time.Time{}
		t.prior = time.Time{}
		return true
	}
	return false
}

func (t *Tracker) State() CandState {
	return CandState{Status: t.status, AliveSince: t.aliveSince, Metric: t.metric()}
}

func (t *Tracker) metric() Metric {
	if len(t.samples) == 0 {
		return Metric{Loss: 1}
	}
	var fails int
	var sum time.Duration
	var oks int
	for _, s := range t.samples {
		if s.ok {
			oks++
			sum += s.rtt
		} else {
			fails++
		}
	}
	m := Metric{Loss: float64(fails) / float64(len(t.samples)), Samples: len(t.samples)}
	if oks > 0 {
		m.RTT = sum / time.Duration(oks)
	}
	return m
}
