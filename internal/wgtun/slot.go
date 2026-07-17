package wgtun

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// Slot owns the default route of one policy routing table, driven
// through the ip(8) command: reads via `ip -json route show table T`,
// writes via the atomic `ip route replace`. exec instead of netlink
// keeps every state change auditable in the logs and trivially
// reproducible by hand.
type Slot struct {
	Table string
	// Via maps a candidate to an optional gateway for its route
	// ("default via GW dev X"); point-to-point wg devices need none,
	// ethernet uplinks in the table=main multi-WAN case do.
	Via map[string]string

	// run is the exec seam for tests; nil means real ip(8).
	run func(ctx context.Context, args ...string) ([]byte, error)
}

func (s *Slot) Describe() string {
	return fmt.Sprintf("default route in table %s", s.Table)
}

func (s *Slot) ip(ctx context.Context, args ...string) ([]byte, error) {
	if s.run != nil {
		return s.run(ctx, args...)
	}
	out, err := exec.CommandContext(ctx, "ip", args...).Output()
	if ee, ok := err.(*exec.ExitError); ok {
		return out, fmt.Errorf("ip %s: %s", strings.Join(args, " "), firstLine(ee.Stderr))
	}
	return out, err
}

type routeEntry struct {
	Dst      string          `json:"dst"`
	Dev      string          `json:"dev"`
	Gateway  string          `json:"gateway"`
	Nexthops json.RawMessage `json:"nexthops"`
}

// Read reports the device owning the table's default route, "" when the
// table has no default (a missing FIB table counts as empty: the kernel
// drops empty tables). A default it cannot attribute to a device (pure
// via, multipath) is reported as an opaque marker string — the engine
// treats any non-roster owner as hands-off, which is exactly right.
func (s *Slot) Read(ctx context.Context) (string, error) {
	out, err := s.ip(ctx, "-json", "route", "show", "table", s.Table)
	if err != nil {
		if strings.Contains(err.Error(), "table does not exist") {
			return "", nil
		}
		return "", err
	}
	var routes []routeEntry
	if len(strings.TrimSpace(string(out))) > 0 {
		if err := json.Unmarshal(out, &routes); err != nil {
			return "", fmt.Errorf("parse ip -json output: %w", err)
		}
	}
	for _, r := range routes {
		if r.Dst != "default" {
			continue
		}
		switch {
		case len(r.Nexthops) > 0:
			return "(multipath)", nil
		case r.Dev != "":
			return r.Dev, nil
		case r.Gateway != "":
			return "via " + r.Gateway, nil
		default:
			return "(unattributed)", nil
		}
	}
	return "", nil
}

func (s *Slot) Activate(ctx context.Context, name string) error {
	args := []string{"route", "replace", "default"}
	if via := s.Via[name]; via != "" {
		args = append(args, "via", via)
	}
	args = append(args, "dev", name, "table", s.Table)
	_, err := s.ip(ctx, args...)
	return err
}

func (s *Slot) Clear(ctx context.Context) error {
	_, err := s.ip(ctx, "route", "del", "default", "table", s.Table)
	if err != nil && (strings.Contains(err.Error(), "No such process") ||
		strings.Contains(err.Error(), "table does not exist")) {
		return nil // already empty
	}
	return err
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if s == "" {
		return "exit status != 0"
	}
	return s
}
