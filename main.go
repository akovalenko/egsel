// Command egsel keeps one active egress channel per slot out of a set
// of interchangeable candidates, with priorities and asymmetric
// hysteresis: instant failover off a dead channel, soak-gated return to
// the preferred one, stickiness among equals.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/akovalenko/egsel/internal/core"
	"github.com/akovalenko/egsel/internal/netdev"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "check":
		err = cmdCheck(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "egsel:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  egsel run    [-c config] [-v]      run the selector daemon
  egsel status [-c config] [-s sock] [-json]
                                     ask a running daemon why it is
                                     where it is
  egsel check  [-c config]           validate the configuration
`)
}

const defaultConfig = "/etc/egsel.toml"

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cpath := fs.String("c", defaultConfig, "config file")
	verbose := fs.Bool("v", false, "debug logging")
	fs.Parse(args)

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cfg, err := loadConfig(*cpath)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var store *core.Store
	if cfg.StateFile != "" {
		store = core.OpenStore(cfg.StateFile, cfg.StateMaxAge.or(10*time.Minute), time.Now(), log)
	}

	var wg sync.WaitGroup
	engines := map[string]*core.Engine{}
	for _, name := range cfg.selectorNames() {
		sc := cfg.Selectors[name]
		slot := &netdev.Slot{Table: sc.Table, Metric: sc.RouteMetric, Via: map[string]string{}}
		for _, c := range sc.Candidates {
			if c.Via != "" {
				slot.Via[c.Name] = c.Via
			}
		}
		eng := core.NewEngine(sc.engineConfig(name), slot, store, log, time.Now)
		engines[name] = eng

		results := make(chan core.ProbeResult, len(sc.Candidates))
		for _, c := range sc.Candidates {
			p := &netdev.Prober{
				Cand:     c.Name,
				Peer:     netip.MustParseAddr(c.Peer),
				Interval: sc.ProbeInterval.or(2 * time.Second),
				Timeout:  sc.ProbeTimeout.or(time.Second),
			}
			wg.Add(1)
			go func() { defer wg.Done(); p.Run(ctx, results) }()
		}
		wg.Add(1)
		go func() { defer wg.Done(); eng.Run(ctx, results) }()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := serveCtl(ctx, cfg.ControlSocket, engines, log); err != nil {
			log.Error("control socket failed", "err", err)
		}
	}()

	if store != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tick := time.NewTicker(30 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					if err := store.Save(time.Now()); err != nil {
						log.Warn("state save failed", "err", err)
					}
				}
			}
		}()
	}

	log.Info("egsel running", "selectors", len(engines), "config", *cpath)
	<-ctx.Done()
	log.Info("shutting down")
	wg.Wait()
	if err := store.Save(time.Now()); err != nil {
		log.Warn("final state save failed", "err", err)
	}
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cpath := fs.String("c", defaultConfig, "config file (to find the socket)")
	sock := fs.String("s", "", "control socket (overrides config)")
	raw := fs.Bool("json", false, "raw JSON")
	fs.Parse(args)

	path := *sock
	if path == "" {
		path = "/run/egsel.sock"
		if cfg, err := loadConfig(*cpath); err == nil {
			path = cfg.ControlSocket
		}
	}
	sts, err := queryStatus(path)
	if err != nil {
		return err
	}
	if *raw {
		enc := jsonEncoder(os.Stdout)
		return enc.Encode(sts)
	}
	renderStatus(os.Stdout, sts)
	return nil
}

func cmdCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	cpath := fs.String("c", defaultConfig, "config file")
	fs.Parse(args)
	cfg, err := loadConfig(*cpath)
	if err != nil {
		return err
	}
	for _, name := range cfg.selectorNames() {
		sc := cfg.Selectors[name]
		ec := sc.engineConfig(name)
		fmt.Printf("selector %s: table %s, fail-%s, window %s, %d candidate(s)\n",
			name, sc.Table, sc.FailMode, ec.Window, len(sc.Candidates))
		for _, c := range ec.Roster {
			fmt.Printf("  %s prio %d/%d\n", c.Name, c.Rank.Prio, c.Rank.Subprio)
		}
	}
	fmt.Println("config OK")
	return nil
}
