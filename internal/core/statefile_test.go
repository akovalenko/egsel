package core

import (
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreRoundtripAndFreshness(t *testing.T) {
	log := slog.Default()
	path := filepath.Join(t.TempDir(), "state.json")
	since := t0.Add(-15 * time.Minute)

	s := OpenStore(path, 10*time.Minute, t0, log)
	s.SetHealthy("freedom", "wgse", since)
	if err := s.Save(t0); err != nil {
		t.Fatal(err)
	}

	// fresh reopen → timer restored
	s2 := OpenStore(path, 10*time.Minute, t0.Add(2*time.Minute), log)
	if got := s2.Prior("freedom", "wgse"); !got.Equal(since) {
		t.Fatalf("fresh file must restore the timer, got %v", got)
	}

	// stale reopen → discarded
	s3 := OpenStore(path, 10*time.Minute, t0.Add(time.Hour), log)
	if got := s3.Prior("freedom", "wgse"); !got.IsZero() {
		t.Fatalf("stale file must be discarded, got %v", got)
	}
}

func TestStoreClearAndMissingFile(t *testing.T) {
	log := slog.Default()
	path := filepath.Join(t.TempDir(), "state.json")
	s := OpenStore(path, 10*time.Minute, t0, log)
	if got := s.Prior("freedom", "wgse"); !got.IsZero() {
		t.Fatalf("missing file must start cold, got %v", got)
	}
	s.SetHealthy("freedom", "wgse", t0)
	s.ClearHealthy("freedom", "wgse")
	if err := s.Save(t0); err != nil {
		t.Fatal(err)
	}
	s2 := OpenStore(path, 10*time.Minute, t0, log)
	if got := s2.Prior("freedom", "wgse"); !got.IsZero() {
		t.Fatalf("cleared timer must not resurrect, got %v", got)
	}
	var nilStore *Store
	if err := nilStore.Save(t0); err != nil {
		t.Fatal("nil store must be a no-op")
	}
	nilStore.SetHealthy("a", "b", t0)
	nilStore.ClearHealthy("a", "b")
	if got := nilStore.Prior("a", "b"); !got.IsZero() {
		t.Fatal("nil store prior must be zero")
	}
}
