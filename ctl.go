package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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

// serveCtl answers on a unix socket, one request per connection:
//
//	status                          the full status dump
//	force [<selector>] <candidate>  switch the slot to a candidate at
//	                                once; automatic policy resumes on the
//	                                next pass
//
// Explainability is a first-class duty of the daemon — this is its mouth.
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
		go answerCtl(ctx, conn, engines, log)
	}
}

// ctlReply is the answer to a force request: the verdict plus the
// post-action status of the affected selector.
type ctlReply struct {
	OK       bool                  `json:"ok"`
	Error    string                `json:"error,omitempty"`
	Statuses []core.SelectorStatus `json:"statuses,omitempty"`
}

// answerCtl serves one connection: read the request line, write one
// reply, close. Connections are answered concurrently — a live-but-
// mute client parks only its own handler (and a dead one EOFs at
// once); the engine serializes cross-goroutine work on its mutex.
func answerCtl(ctx context.Context, conn net.Conn, engines map[string]*core.Engine, log *slog.Logger) {
	defer conn.Close()
	req, err := readRequest(conn)
	if err != nil {
		writeJSON(conn, ctlReply{Error: err.Error()}, log)
		return
	}
	parts := strings.Fields(req)
	var rep ctlReply
	switch {
	case req == "status":
		writeJSON(conn, statuses(engines), log) // bare array, as ever
		return
	case parts[0] == "force" && len(parts) == 2:
		rep = ctlForce(ctx, engines, "", parts[1])
	case parts[0] == "force" && len(parts) == 3:
		rep = ctlForce(ctx, engines, parts[1], parts[2])
	default:
		rep = ctlReply{Error: fmt.Sprintf("unknown request %q", req)}
	}
	writeJSON(conn, rep, log)
}

// readRequest reads one command line; a dead client shows up as EOF,
// and a line without its trailing newline still counts.
func readRequest(conn net.Conn) (string, error) {
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil && line == "" {
		return "", errors.New("no request: one line expected, e.g. \"status\"")
	}
	return strings.TrimSpace(line), nil
}

func writeJSON(w io.Writer, v any, log *slog.Logger) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Warn("control write", "err", err)
	}
}

// ctlForce forces one selector to cand. An empty selector means
// "resolve cand across selectors": a bare candidate must live in
// exactly one roster.
func ctlForce(ctx context.Context, engines map[string]*core.Engine, selector, cand string) ctlReply {
	eng, err := pickEngine(engines, selector, cand)
	if err != nil {
		return ctlReply{Error: err.Error()}
	}
	if err := eng.Force(ctx, cand); err != nil {
		return ctlReply{Error: err.Error()}
	}
	return ctlReply{OK: true, Statuses: []core.SelectorStatus{eng.Snapshot()}}
}

// pickEngine resolves a force target: by selector name, or by a
// candidate unique across rosters.
func pickEngine(engines map[string]*core.Engine, selector, cand string) (*core.Engine, error) {
	if selector != "" {
		eng := engines[selector]
		if eng == nil {
			return nil, fmt.Errorf("no such selector %q (have: %s)", selector, strings.Join(selectorNames(engines), ", "))
		}
		if !eng.HasCandidate(cand) {
			return nil, fmt.Errorf("no candidate %q in selector %s", cand, selector)
		}
		return eng, nil
	}
	var match *core.Engine
	for _, n := range selectorNames(engines) {
		if engines[n].HasCandidate(cand) {
			if match != nil {
				return nil, fmt.Errorf("candidate %q lives in several selectors; name one: force <selector> <candidate>", cand)
			}
			match = engines[n]
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no candidate %q in any selector", cand)
	}
	return match, nil
}

func selectorNames(engines map[string]*core.Engine) []string {
	names := make([]string, 0, len(engines))
	for n := range engines {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func statuses(engines map[string]*core.Engine) []core.SelectorStatus {
	names := selectorNames(engines)
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

// queryStatus fetches the status dump from a running daemon. Pure
// blocking local IPC — no deadlines: a dead daemon fails the dial at
// once, a wedged one blocks visibly until Ctrl-C or an external
// timeout(1), which is the admin's call, not an arbitrary constant's.
// The request line is for protocol daemons; a pre-force one ignores it
// and dumps the array anyway.
func queryStatus(path string) ([]core.SelectorStatus, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("is the daemon running? %w", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "status\n"); err != nil {
		return nil, err
	}
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

// queryCtl sends one force request and reads the reply. Blocking local
// IPC, no deadlines — see queryStatus.
func queryCtl(path, req string) (ctlReply, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return ctlReply{}, fmt.Errorf("is the daemon running? %w", err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, req+"\n"); err != nil {
		return ctlReply{}, err
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		return ctlReply{}, err
	}
	// a bare array is a pre-force daemon's status dump: it ignores the
	// request line entirely
	if len(raw) > 0 && raw[0] == '[' {
		return ctlReply{}, errors.New("daemon is older than this command; upgrade and restart egsel")
	}
	var rep ctlReply
	if err := json.Unmarshal(raw, &rep); err != nil {
		return ctlReply{}, fmt.Errorf("garbled daemon reply: %w", err)
	}
	return rep, nil
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
		if !st.ForcedAt.IsZero() {
			fmt.Fprintf(w, "  forced: at %s — challengers re-soak from here\n", st.ForcedAt.Format(time.TimeOnly))
		}
		fmt.Fprintf(w, "  decision: %s\n", st.LastDecision)
		if !st.SeedDeadline.IsZero() {
			fmt.Fprintf(w, "  seed deadline: %s\n", st.SeedDeadline.Format(time.TimeOnly))
		}
		if !st.GraceDeadline.IsZero() {
			fmt.Fprintf(w, "  fresh-evidence grace: until %s\n", st.GraceDeadline.Format(time.TimeOnly))
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
