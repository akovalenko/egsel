package netdev

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func fakeIP(t *testing.T, wantArgs string, out string, err error) func(context.Context, ...string) ([]byte, error) {
	return func(_ context.Context, args ...string) ([]byte, error) {
		t.Helper()
		if got := strings.Join(args, " "); got != wantArgs {
			t.Fatalf("ip args = %q, want %q", got, wantArgs)
		}
		return []byte(out), err
	}
}

func TestReadOwner(t *testing.T) {
	s := &Slot{Table: "freedom"}
	s.run = fakeIP(t, "-json route show table freedom default",
		`[{"dst":"default","dev":"wgse2","flags":[],"scope":"link"}]`, nil)
	dev, err := s.Read(context.Background())
	if err != nil || dev != "wgse2" {
		t.Fatalf("got %q, %v", dev, err)
	}
}

func TestReadEmptyAndMissingTable(t *testing.T) {
	s := &Slot{Table: "freedom"}
	s.run = fakeIP(t, "-json route show table freedom default", "[]", nil)
	if dev, err := s.Read(context.Background()); err != nil || dev != "" {
		t.Fatalf("empty table: got %q, %v", dev, err)
	}
	s.run = fakeIP(t, "-json route show table freedom default", "",
		errors.New(`ip -json route show table freedom default: Error: ipv4: FIB table does not exist.`))
	if dev, err := s.Read(context.Background()); err != nil || dev != "" {
		t.Fatalf("missing table must read as empty: got %q, %v", dev, err)
	}
}

func TestReadMetricIdentity(t *testing.T) {
	s := &Slot{Table: "main", Metric: 100}
	s.run = fakeIP(t, "-json route show table main default",
		`[{"dst":"default","gateway":"10.0.0.1","dev":"ppp0","metric":50},
		  {"dst":"default","dev":"wgse","metric":100},
		  {"dst":"default","dev":"enp6s0","metric":600}]`, nil)
	dev, err := s.Read(context.Background())
	if err != nil || dev != "wgse" {
		t.Fatalf("must pick our metric: got %q, %v", dev, err)
	}
	// no default at our metric, only foreign ones → our slot is empty
	s.run = fakeIP(t, "-json route show table main default",
		`[{"dst":"default","dev":"ppp0","metric":50}]`, nil)
	if dev, _ := s.Read(context.Background()); dev != "" {
		t.Fatalf("foreign-only defaults: got %q", dev)
	}
}

func TestReadOpaqueForms(t *testing.T) {
	s := &Slot{Table: "t"}
	s.run = fakeIP(t, "-json route show table t default",
		`[{"dst":"default","gateway":"10.0.0.1"}]`, nil)
	if dev, _ := s.Read(context.Background()); dev != "via 10.0.0.1" {
		t.Fatalf("pure-via default: got %q", dev)
	}
	s.run = fakeIP(t, "-json route show table t default",
		`[{"dst":"default","nexthops":[{"dev":"a"},{"dev":"b"}]}]`, nil)
	if dev, _ := s.Read(context.Background()); dev != "(multipath)" {
		t.Fatalf("multipath default: got %q", dev)
	}
}

func TestActivateVariants(t *testing.T) {
	s := &Slot{Table: "freedom"}
	s.run = fakeIP(t, "route replace default dev wgse table freedom", "", nil)
	if err := s.Activate(context.Background(), "wgse"); err != nil {
		t.Fatal(err)
	}
	s = &Slot{Table: "main", Metric: 100, Via: map[string]string{"enp6s0": "192.168.1.1"}}
	s.run = fakeIP(t, "route replace default via 192.168.1.1 dev enp6s0 table main metric 100", "", nil)
	if err := s.Activate(context.Background(), "enp6s0"); err != nil {
		t.Fatal(err)
	}
}

func TestClearTolerant(t *testing.T) {
	s := &Slot{Table: "freedom", Metric: 100}
	s.run = fakeIP(t, "route del default table freedom metric 100", "",
		errors.New("ip route del default table freedom metric 100: RTNETLINK answers: No such process"))
	if err := s.Clear(context.Background()); err != nil {
		t.Fatalf("clearing an empty slot must succeed: %v", err)
	}
}
