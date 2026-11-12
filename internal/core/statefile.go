package core

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store persists per-candidate healthy-since marks across daemon
// restarts, so frequent redeploys do not starve a pending upgrade (the
// soak clock survives a short downtime). The slot owner is deliberately
// NOT stored: the kernel is the source of truth for it (adopt-or-seed);
// the file is authoritative only for the accumulated timers.
type Store struct {
	path   string
	maxAge time.Duration

	mu   sync.Mutex
	data fileData
}

type fileData struct {
	SavedAt time.Time `json:"saved_at"`
	// selector -> candidate -> continuously-healthy-since
	Selectors map[string]map[string]time.Time `json:"selectors"`
	// selector -> moment of the last admin force ("egsel force"): the
	// trust-episode boundary, surviving restarts under the same
	// freshness guard as the timers
	ForcedAt map[string]time.Time `json:"forced_at,omitempty"`
}

// OpenStore loads path if it exists and is fresh enough; a stale file
// (the daemon was down long — the world may have changed) or a corrupt
// one is discarded and the store starts empty.
func OpenStore(path string, maxAge time.Duration, now time.Time, log *slog.Logger) *Store {
	s := &Store{path: path, maxAge: maxAge}
	s.data.Selectors = map[string]map[string]time.Time{}
	raw, err := os.ReadFile(path)
	switch {
	case err != nil:
		if !os.IsNotExist(err) {
			log.Warn("state file unreadable, starting cold", "path", path, "err", err)
		}
	default:
		var d fileData
		if err := json.Unmarshal(raw, &d); err != nil {
			log.Warn("state file corrupt, starting cold", "path", path, "err", err)
		} else if age := now.Sub(d.SavedAt); age > maxAge {
			log.Info("state file stale, starting cold", "path", path, "age", age.Round(time.Second), "max_age", maxAge)
		} else {
			if d.Selectors != nil {
				s.data.Selectors = d.Selectors
			}
			s.data.ForcedAt = d.ForcedAt
			log.Info("state file loaded", "path", path, "age", age.Round(time.Second))
		}
	}
	return s
}

// Prior returns the restored healthy-since for a candidate, zero if none.
func (s *Store) Prior(selector, cand string) time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Selectors[selector][cand]
}

func (s *Store) SetHealthy(selector, cand string, since time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.data.Selectors[selector]
	if m == nil {
		m = map[string]time.Time{}
		s.data.Selectors[selector] = m
	}
	m[cand] = since
}

func (s *Store) ClearHealthy(selector, cand string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data.Selectors[selector], cand)
}

// PriorForceAt returns the restored admin-force moment, zero if none.
func (s *Store) PriorForceAt(selector string) time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.ForcedAt[selector]
}

func (s *Store) SetForceAt(selector string, at time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.ForcedAt == nil {
		s.data.ForcedAt = map[string]time.Time{}
	}
	s.data.ForcedAt[selector] = at
}

// Save writes the file atomically (tmp + rename). It always rewrites,
// even with unchanged timers: SavedAt doubles as the daemon's liveness
// mark, and the load-time freshness check keys off it.
func (s *Store) Save(now time.Time) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data.SavedAt = now
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("rename %s: %w", tmp, err)
	}
	return nil
}
