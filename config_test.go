package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "egsel.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const goodConfig = `
state_file = "/var/lib/egsel/state.json"

[selectors.freedom]
transport = "netdev"
table = "freedom"
fail_mode = "open"
upgrade_window = "30m"

[[selectors.freedom.candidates]]
name = "wgse"
peer = "172.18.0.1"
prio = 100

[[selectors.freedom.candidates]]
name = "wgse2"
peer = "172.20.0.1"
prio = 50
subprio = 10
`

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, goodConfig))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ControlSocket != "/run/egsel.sock" {
		t.Fatalf("default socket: %q", cfg.ControlSocket)
	}
	ec := cfg.Selectors["freedom"].engineConfig("freedom")
	if ec.Health.UpThreshold != 1 || ec.Health.DownThreshold != 3 || ec.Health.MetricWindow != 30 {
		t.Fatalf("health defaults: %+v", ec.Health)
	}
	if ec.Window != 30*time.Minute || ec.SeedBudget != 30*time.Second || ec.LossTol != 0.05 {
		t.Fatalf("policy defaults: %+v", ec)
	}
	if len(ec.Roster) != 2 || ec.Roster[0].Name != "wgse" || ec.Roster[1].Rank.Subprio != 10 {
		t.Fatalf("roster: %+v", ec.Roster)
	}
}

func TestLoadConfigRejects(t *testing.T) {
	cases := []struct{ name, patch, wantErr string }{
		{"missing fail_mode", `fail_mode = "open"`, "fail_mode must be explicitly"},
		{"bad transport", `transport = "netdev"`, "transport must be"},
		{"bad peer", `peer = "172.18.0.1"`, "peer must be an IPv4"},
		{"unknown key", `table = "freedom"`, "unknown config key"},
	}
	broken := map[string]string{
		"missing fail_mode": `fail_mode = ""`,
		"bad transport":     `transport = "carrier-pigeon"`,
		"bad peer":          `peer = "not-an-ip"`,
		"unknown key":       "table = \"freedom\"\nsurprise = 1",
	}
	for _, c := range cases {
		body := strings.Replace(goodConfig, c.patch, broken[c.name], 1)
		if body == goodConfig {
			t.Fatalf("%s: patch %q not found in template", c.name, c.patch)
		}
		_, err := loadConfig(writeConfig(t, body))
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Fatalf("%s: got %v, want %q", c.name, err, c.wantErr)
		}
	}
}

func TestLoadConfigDuplicateCandidate(t *testing.T) {
	body := strings.Replace(goodConfig, `name = "wgse2"`, `name = "wgse"`, 1)
	if _, err := loadConfig(writeConfig(t, body)); err == nil ||
		!strings.Contains(err.Error(), "duplicate candidate") {
		t.Fatalf("got %v", err)
	}
}
