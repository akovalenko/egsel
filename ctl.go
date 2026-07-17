package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/akovalenko/egsel/internal/core"
)

// serveCtl answers status queries on a unix socket: each connection
// receives the full status JSON and is closed. Explainability is a
// first-class duty of the daemon — this is its mouth.
func serveCtl(ctx context.Context, path string, engines map[string]*core.Engine, log *slog.Logger) error {
	_ = os.Remove(path) // a stale socket from a dead daemon
	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("control socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("control socket perms: %w", err)
	}
	go func() {
		<-ctx.Done()
		l.Close()
		os.Remove(path)
	}()
	log.Info("control socket ready", "path", path)
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			log.Warn("control accept", "err", err)
			continue
		}
		enc := json.NewEncoder(conn)
		enc.SetIndent("", "  ")
		if err := enc.Encode(statuses(engines)); err != nil {
			log.Warn("control write", "err", err)
		}
		conn.Close()
	}
}

func statuses(engines map[string]*core.Engine) []core.SelectorStatus {
	names := make([]string, 0, len(engines))
	for n := range engines {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]core.SelectorStatus, 0, len(names))
	for _, n := range names {
		out = append(out, engines[n].Snapshot())
	}
	return out
}

func jsonEncoder(w io.Writer) *json.Encoder {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc
}

// queryStatus fetches the status dump from a running daemon.
func queryStatus(path string) ([]core.SelectorStatus, error) {
	conn, err := net.DialTimeout("unix", path, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("is the daemon running? %w", err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	raw, err := io.ReadAll(conn)
	if err != nil {
		return nil, err
	}
	var st []core.SelectorStatus
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return st, nil
}

// renderStatus prints the human answer to "why is it on that channel?".
func renderStatus(w io.Writer, sts []core.SelectorStatus) {
	for i, st := range sts {
		if i > 0 {
			fmt.Fprintln(w)
		}
		mode := "fail-" + st.FailMode
		fmt.Fprintf(w, "selector %s (%s), %s, window %s\n", st.Name, st.Slot, mode, st.Window)
		cur := st.Current
		if cur == "" {
			cur = "(none)"
		}
		note := ""
		if st.Opaque {
			note = " [outside roster — held, not managed]"
		}
		since := ""
		if !st.CurrentSince.IsZero() {
			since = " since " + st.CurrentSince.Format(time.TimeOnly)
		}
		fmt.Fprintf(w, "  current: %s%s%s\n", cur, since, note)
		fmt.Fprintf(w, "  decision: %s\n", st.LastDecision)
		if !st.SeedDeadline.IsZero() {
			fmt.Fprintf(w, "  seed deadline: %s\n", st.SeedDeadline.Format(time.TimeOnly))
		}
		if st.SlotError != "" {
			fmt.Fprintf(w, "  SLOT ERROR: %s\n", st.SlotError)
		}
		nameW, verW := 0, 0
		for _, c := range st.Candidates {
			nameW = max(nameW, len(c.Name))
			verW = max(verW, len(c.Status))
		}
		for _, c := range st.Candidates {
			metric := "no samples"
			if c.Samples > 0 {
				metric = fmt.Sprintf("loss %d%%", int(c.Loss*100+0.5))
				if c.RTTMs > 0 {
					metric += fmt.Sprintf(" rtt %.1fms", c.RTTMs)
				}
			}
			fmt.Fprintf(w, "    %-*s  prio %3d/%-3d  %-*s  %-22s  %s\n",
				nameW, c.Name, c.Prio, c.Subprio, verW, strings.ToUpper(c.Status), metric, c.Verdict)
		}
	}
}
