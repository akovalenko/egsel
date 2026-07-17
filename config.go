package main

import (
	"cmp"
	"fmt"
	"net/netip"
	"sort"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/akovalenko/egsel/internal/core"
)

// duration lets TOML carry "30m"-style values.
type duration time.Duration

func (d *duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = duration(v)
	return nil
}

func (d duration) or(def time.Duration) time.Duration {
	return cmp.Or(time.Duration(d), def)
}

type Config struct {
	StateFile     string   `toml:"state_file"`
	StateMaxAge   duration `toml:"state_max_age"`
	ControlSocket string   `toml:"control_socket"`

	Selectors map[string]*SelectorConfig `toml:"selectors"`
}

type SelectorConfig struct {
	Transport string `toml:"transport"`
	Table     string `toml:"table"` // defaults to "main"
	// RouteMetric pins our default's identity to a metric (routes are
	// keyed by dst+metric), letting admins layer their own defaults
	// around ours; 0 = a plain metricless default.
	RouteMetric int `toml:"route_metric"`
	// FailMode is a required, explicit policy choice: "open" (no
	// reachable candidate -> clear the slot, traffic falls through to
	// remaining routing) or "closed" (point it at the best static rank
	// -- a dead route beats none when the slot is the only default).
	FailMode string `toml:"fail_mode"`

	ProbeInterval duration `toml:"probe_interval"`
	ProbeTimeout  duration `toml:"probe_timeout"`
	UpThreshold   int      `toml:"up_threshold"`
	DownThreshold int      `toml:"down_threshold"`
	UpgradeWindow duration `toml:"upgrade_window"`
	SeedBudget    duration `toml:"seed_budget"`
	MetricWindow  int      `toml:"metric_window"`
	LossTolerance float64  `toml:"loss_tolerance"`

	Candidates []CandidateConfig `toml:"candidates"`
}

type CandidateConfig struct {
	Name    string `toml:"name"`
	Peer    string `toml:"peer"`
	Via     string `toml:"via"` // optional gateway for the route
	Prio    int    `toml:"prio"`
	Subprio int    `toml:"subprio"`
}

func loadConfig(path string) (*Config, error) {
	var cfg Config
	md, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, err
	}
	if un := md.Undecoded(); len(un) > 0 {
		return nil, fmt.Errorf("unknown config key %q", un[0].String())
	}
	if cfg.ControlSocket == "" {
		cfg.ControlSocket = "/run/egsel.sock"
	}
	if len(cfg.Selectors) == 0 {
		return nil, fmt.Errorf("no selectors configured")
	}
	for name, sc := range cfg.Selectors {
		sc.Table = cmp.Or(sc.Table, "main")
		if err := sc.validate(); err != nil {
			return nil, fmt.Errorf("selector %s: %w", name, err)
		}
	}
	return &cfg, nil
}

func (sc *SelectorConfig) validate() error {
	if sc.Transport != "netdev" {
		return fmt.Errorf("transport must be \"netdev\" (got %q); socks is planned", sc.Transport)
	}
	if sc.RouteMetric < 0 {
		return fmt.Errorf("route_metric must be ≥ 0")
	}
	if sc.FailMode != "open" && sc.FailMode != "closed" {
		return fmt.Errorf("fail_mode must be explicitly \"open\" or \"closed\" (got %q)", sc.FailMode)
	}
	if len(sc.Candidates) == 0 {
		return fmt.Errorf("at least one candidate is required")
	}
	if sc.LossTolerance < 0 || sc.LossTolerance >= 1 {
		return fmt.Errorf("loss_tolerance must be in [0, 1)")
	}
	seen := map[string]bool{}
	for i := range sc.Candidates {
		c := &sc.Candidates[i]
		if c.Name == "" {
			return fmt.Errorf("candidate #%d: name is required", i+1)
		}
		if seen[c.Name] {
			return fmt.Errorf("duplicate candidate %q", c.Name)
		}
		seen[c.Name] = true
		a, err := netip.ParseAddr(c.Peer)
		if err != nil || !a.Is4() {
			return fmt.Errorf("candidate %s: peer must be an IPv4 address (got %q)", c.Name, c.Peer)
		}
		if c.Via != "" {
			if a, err := netip.ParseAddr(c.Via); err != nil || !a.Is4() {
				return fmt.Errorf("candidate %s: via must be an IPv4 address (got %q)", c.Name, c.Via)
			}
		}
	}
	return nil
}

func (sc *SelectorConfig) failMode() core.FailMode {
	if sc.FailMode == "closed" {
		return core.FailClosed
	}
	return core.FailOpen
}

func (sc *SelectorConfig) engineConfig(name string) core.EngineConfig {
	roster := make([]core.Candidate, len(sc.Candidates))
	for i, c := range sc.Candidates {
		roster[i] = core.Candidate{Name: c.Name, Rank: core.Rank{Prio: c.Prio, Subprio: c.Subprio}}
	}
	return core.EngineConfig{
		Name:   name,
		Roster: roster,
		Health: core.HealthConfig{
			UpThreshold:   cmp.Or(sc.UpThreshold, 1),
			DownThreshold: cmp.Or(sc.DownThreshold, 3),
			MetricWindow:  cmp.Or(sc.MetricWindow, 30),
		},
		Window:         sc.UpgradeWindow.or(30 * time.Minute),
		SeedBudget:     sc.SeedBudget.or(30 * time.Second),
		FailMode:       sc.failMode(),
		LossTol:        cmp.Or(sc.LossTolerance, 0.05),
		ReconcileEvery: sc.ProbeInterval.or(2 * time.Second),
	}
}

// selectorNames returns config keys in stable order.
func (c *Config) selectorNames() []string {
	names := make([]string, 0, len(c.Selectors))
	for n := range c.Selectors {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
