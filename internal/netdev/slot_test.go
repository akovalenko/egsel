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
	dev, note, err := s.Read(context.Background())
	if err != nil || dev != "wgse2" || note != "" {
		t.Fatalf("got %q, %q, %v", dev, note, err)
	}
}

func TestReadEmptyAndMissingTable(t *testing.T) {
	s := &Slot{Table: "freedom"}
	s.run = fakeIP(t, "-json route show table freedom default", "[]", nil)
	if dev, _, err := s.Read(context.Background()); err != nil || dev != "" {
		t.Fatalf("empty table: got %q, %v", dev, err)
	}
	s.run = fakeIP(t, "-json route show table freedom default", "",
		errors.New(`ip -json route show table freedom default: Error: ipv4: FIB table does not exist.`))
	if dev, _, err := s.Read(context.Background()); err != nil || dev != "" {
		t.Fatalf("missing table must read as empty: got %q, %v", dev, err)
	}
}

func TestReadMetricIdentityAndShadow(t *testing.T) {
	s := &Slot{Table: "main", Metric: 100}
	s.run = fakeIP(t, "-json route show table main default",
		`[{"dst":"default","gateway":"10.0.0.1","dev":"ppp0","metric":50},
		  {"dst":"default","dev":"wgse","metric":100},
		  {"dst":"default","dev":"enp6s0","metric":600}]`, nil)
	dev, note, err := s.Read(context.Background())
	if err != nil || dev != "wgse" {
		t.Fatalf("must pick our metric: got %q, %v", dev, err)
	}
	if note != "shadowed by foreign default dev ppp0 metric 50" {
		t.Fatalf("lower-metric foreign default must surface: %q", note)
	}
	// no default at our metric, only foreign ones → empty slot + note
	s.run = fakeIP(t, "-json route show table main default",
		`[{"dst":"default","dev":"ppp0","metric":50}]`, nil)
	dev, note, _ = s.Read(context.Background())
	if dev != "" || note == "" {
		t.Fatalf("foreign-only defaults: got %q, %q", dev, note)
	}
	// higher-metric foreign default is normal coexistence — no note
	s.run = fakeIP(t, "-json route show table main default",
		`[{"dst":"default","dev":"wgse","metric":100},
		  {"dst":"default","dev":"enp6s0","metric":600}]`, nil)
	dev, note, _ = s.Read(context.Background())
	if dev != "wgse" || note != "" {
		t.Fatalf("higher-metric neighbors are fine: got %q, %q", dev, note)
	}
}

func TestReadOpaqueForms(t *testing.T) {
	s := &Slot{Table: "t"}
	s.run = fakeIP(t, "-json route show table t default",
		`[{"dst":"default","gateway":"10.0.0.1"}]`, nil)
	if dev, _, _ := s.Read(context.Background()); dev != "via 10.0.0.1" {
		t.Fatalf("pure-via default: got %q", dev)
	}
	s.run = fakeIP(t, "-json route show table t default",
		`[{"dst":"default","nexthops":[{"dev":"a"},{"dev":"b"}]}]`, nil)
	if dev, _, _ := s.Read(context.Background()); dev != "(multipath)" {
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

func TestProbePacketRoundtrip(t *testing.T) {
	req := echoRequest(0x1234, 7)
	if checksum(req) != 0 {
		t.Fatal("checksum over a checksummed message must be zero")
	}
	// synthesize the reply as the kernel would deliver it: IP header +
	// ICMP with type flipped to echo-reply and checksum fixed up
	reply := append(make([]byte, 20), req...)
	reply[0] = 0x45
	icmp := reply[20:]
	icmp[0] = 0
	icmp[2], icmp[3] = 0, 0
	sum := checksum(icmp)
	icmp[2], icmp[3] = byte(sum>>8), byte(sum)
	if got := isOurEchoReply(reply, 0x1234, 7); got != true {
		t.Fatal("must match our reply")
	}
	if isOurEchoReply(reply, 0x1234, 8) {
		t.Fatal("wrong seq must not match")
	}
	if isOurEchoReply(reply[:15], 0x1234, 7) {
		t.Fatal("short packet must not match")
	}
}
