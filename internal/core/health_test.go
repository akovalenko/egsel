package core

import (
	"testing"
	"time"
)

func probeAt(ok bool, at time.Time) ProbeResult {
	return ProbeResult{Cand: "c", OK: ok, RTT: 40 * time.Millisecond, At: at}
}

func TestTrackerDebounce(t *testing.T) {
	tr := NewTracker(HealthConfig{UpThreshold: 1, DownThreshold: 3, MetricWindow: 10}, time.Time{})
	if got := tr.State().Status; got != Unknown {
		t.Fatalf("must start unknown, got %v", got)
	}
	if !tr.Observe(probeAt(true, t0)) {
		t.Fatal("one success should flip unknown→alive with up=1")
	}
	// two misses: below down threshold — still alive, clock not reset
	tr.Observe(probeAt(false, t0.Add(2*time.Second)))
	tr.Observe(probeAt(false, t0.Add(4*time.Second)))
	s := tr.State()
	if s.Status != Alive || !s.AliveSince.Equal(t0) {
		t.Fatalf("sub-threshold misses must not reset the alive clock: %+v", s)
	}
	// third miss → dead, clock cleared
	if !tr.Observe(probeAt(false, t0.Add(6*time.Second))) {
		t.Fatal("third consecutive miss should flip alive→dead")
	}
	if s := tr.State(); s.Status != Dead || !s.AliveSince.IsZero() {
		t.Fatalf("dead must clear alive-since: %+v", s)
	}
	// recovery restarts the clock at the new transition
	tr.Observe(probeAt(true, t0.Add(10*time.Second)))
	if s := tr.State(); s.Status != Alive || !s.AliveSince.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("recovery must restart the clock: %+v", s)
	}
}

func TestTrackerPriorAdoptedOnDirectAlive(t *testing.T) {
	prior := t0.Add(-20 * time.Minute)
	tr := NewTracker(HealthConfig{UpThreshold: 1, DownThreshold: 3, MetricWindow: 10}, prior)
	tr.Observe(probeAt(true, t0))
	if s := tr.State(); !s.AliveSince.Equal(prior) {
		t.Fatalf("direct unknown→alive must adopt the restored clock: %+v", s)
	}
}

func TestTrackerPriorDroppedAfterDeath(t *testing.T) {
	prior := t0.Add(-20 * time.Minute)
	tr := NewTracker(HealthConfig{UpThreshold: 1, DownThreshold: 2, MetricWindow: 10}, prior)
	tr.Observe(probeAt(false, t0))
	tr.Observe(probeAt(false, t0.Add(2*time.Second))) // → dead: continuity broken
	tr.Observe(probeAt(true, t0.Add(4*time.Second)))
	if s := tr.State(); !s.AliveSince.Equal(t0.Add(4 * time.Second)) {
		t.Fatalf("prior must be dropped once proven dead: %+v", s)
	}
}

func TestTrackerMetricWindow(t *testing.T) {
	tr := NewTracker(HealthConfig{UpThreshold: 1, DownThreshold: 3, MetricWindow: 4}, time.Time{})
	at := t0
	feed := func(ok bool, rtt time.Duration) {
		tr.Observe(ProbeResult{Cand: "c", OK: ok, RTT: rtt, At: at})
		at = at.Add(time.Second)
	}
	feed(false, 0) // will slide out of the window
	feed(true, 40*time.Millisecond)
	feed(true, 60*time.Millisecond)
	feed(false, 0)
	feed(true, 50*time.Millisecond)
	m := tr.State().Metric
	if m.Samples != 4 {
		t.Fatalf("window must cap samples: %+v", m)
	}
	if m.Loss != 0.25 {
		t.Fatalf("loss over the window: %+v", m)
	}
	if m.RTT != 50*time.Millisecond {
		t.Fatalf("mean RTT of successes: %+v", m)
	}
}

func TestMetricCompare(t *testing.T) {
	a := Metric{Loss: 0, RTT: 50 * time.Millisecond, Samples: 30}
	b := Metric{Loss: 0.5, RTT: 10 * time.Millisecond, Samples: 30}
	if a.Compare(b, 0.05) <= 0 {
		t.Fatal("loss outranks RTT")
	}
	c := Metric{Loss: 0.04, RTT: 30 * time.Millisecond, Samples: 30}
	if a.Compare(c, 0.05) >= 0 {
		t.Fatal("within tolerance RTT decides")
	}
	noData := Metric{Loss: 0, Samples: 0}
	if a.Compare(noData, 0.05) <= 0 {
		t.Fatal("no RTT data loses to a measurement")
	}
}
