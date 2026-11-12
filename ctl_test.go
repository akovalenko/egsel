package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/akovalenko/egsel/internal/core"
)

// ctlSlot is a kernel-table stand-in for control-socket tests.
type ctlSlot struct{ owner string }

func (f *ctlSlot) Read(context.Context) (string, error)       { return f.owner, nil }
func (f *ctlSlot) Activate(_ context.Context, n string) error { f.owner = n; return nil }
func (f *ctlSlot) Clear(context.Context) error                { f.owner = ""; return nil }
func (f *ctlSlot) Describe() string                           { return "fake slot" }

// startCtlDaemon serves one engine on a throwaway socket.
func startCtlDaemon(t *testing.T) (string, *ctlSlot) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	slot := &ctlSlot{owner: "wgse"}
	eng := core.NewEngine(core.EngineConfig{
		Name: "freedom",
		Roster: []core.Candidate{
			{Name: "wgse", Rank: core.Rank{Prio: 100}},
			{Name: "wgie", Rank: core.Rank{Prio: 50}},
		},
		Health: core.HealthConfig{UpThreshold: 1, DownThreshold: 3, MetricWindow: 10},
		Window: time.Minute, SeedBudget: time.Second,
		FailMode: core.FailOpen, ReconcileEvery: time.Hour,
	}, slot, nil, slog.Default(), func() time.Time { return time.Now() })
	sock := filepath.Join(t.TempDir(), "egsel.sock")
	go serveCtl(ctx, sock, map[string]*core.Engine{"freedom": eng}, slog.Default())
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			return sock, slot
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("control socket did not come up")
	return "", nil
}

func TestCtlForceRoundTrip(t *testing.T) {
	sock, slot := startCtlDaemon(t)

	rep, err := queryCtl(sock, "force wgie")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || len(rep.Statuses) != 1 {
		t.Fatalf("reply: %+v", rep)
	}
	st := rep.Statuses[0]
	if st.Current != "wgie" || slot.owner != "wgie" || st.ForcedAt.IsZero() {
		t.Fatalf("force must switch at once: %+v slot=%s", st, slot.owner)
	}

	// unknown candidate: honest error, slot untouched
	rep, err = queryCtl(sock, "force nope")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.Error == "" || slot.owner != "wgie" {
		t.Fatalf("unknown candidate must be rejected: %+v", rep)
	}

	// selector-qualified form
	rep, err = queryCtl(sock, "force freedom wgse")
	if err != nil {
		t.Fatal(err)
	}
	if !rep.OK || slot.owner != "wgse" {
		t.Fatalf("qualified force: %+v slot=%s", rep, slot.owner)
	}

	// garbage: a clean error, not a crash
	rep, err = queryCtl(sock, "bogus")
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK || rep.Error == "" {
		t.Fatalf("bogus request must fail cleanly: %+v", rep)
	}
}

func TestCtlSilentClientGetsStatus(t *testing.T) {
	sock, _ := startCtlDaemon(t)
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	var st []core.SelectorStatus
	if err := json.Unmarshal(raw, &st); err != nil || len(st) != 1 {
		t.Fatalf("a silent client must get the bare status array: %q", raw)
	}
}
