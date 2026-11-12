# egsel — priority failover selector for egress channels

egsel keeps **one active egress channel per slot** out of a set of
interchangeable candidates — e.g. several VPN tunnels (or WAN uplinks)
competing for a default route. What sets it apart from "route with the
best metric" tools is **asymmetric hysteresis**:

- **Failover** off a dead channel is *mandatory* and instant — the cost
  (broken connections) is already paid. Only a debounce guards against a
  single lost probe.
- **Upgrade** away from a *working* channel is *optional* and costs
  broken connections, so it takes strong proof: a strictly
  higher-priority candidate, continuously alive for a soak window
  (default 30 min). Equals never displace each other — if two fallbacks
  are both alive and you sit on one, you stay.

The decision depends on where you already are, not just on the current
liveness vector: it is a state machine with memory, which is why plain
metric routing or happy-eyeballs racing (both of which jump to the best
channel the moment it appears) do not fit.

## Policy: `(prio, subprio, metric)`

Each candidate carries a lexicographic key; each part does one job:

| key | source | role |
|---|---|---|
| `prio` | config | the **only preemption axis**: strictly higher prio, soaked for the window, takes the slot |
| `subprio` | config | static tiebreaker at *forced* choices (seed, failover); never preempts |
| `metric` | measured | dynamic tiebreaker: probe loss, then mean RTT (within a loss tolerance); never preempts |

A typical layout: one preferred channel at high prio (the only one
allowed to evict others), fallbacks sharing a lower prio — sticky among
themselves, with `subprio` expressing a mild lean.

## Liveness: measure what you use

There is no such thing as abstract host liveness — censorship and
DDoS-protection hit protocols selectively. Each candidate is probed over
the very (protocol, path) pair that will carry traffic: an ICMP echo
with the socket bound to the candidate interface (`SO_BINDTODEVICE`),
deliberately independent of policy routing so a broken routing config
cannot read as a dead channel. For a WireGuard tunnel one echo to the
far tunnel address checks the crypto, the underlay and the peer at once.

The same probe stream feeds both signals: the debounced alive/dead
verdict (up/down thresholds) and the sliding loss/RTT metric.

## Slots and transports

The engine is transport-blind; a transport supplies the probe and the
slot. The `netdev` transport (the only one so far; a SOCKS front is
planned) owns **a default route of a routing table** (`table` defaults
to `main`), driven through `ip(8)`: `ip -json route show ... default`
to read, the atomic `ip route replace` to switch. Candidates are plain
L3 interfaces — WireGuard tunnels, ethernet uplinks (add `via` for a
gateway route), anything with a name and something to ping through it.

Kernel routes are keyed by `(dst, metric)`, so an optional
`route_metric` pins the identity of *our* default: egsel manipulates
only the default at its metric while admins freely layer their own
around it — a hand-run pppd at metric 50 wins while it lives, and when
it dies traffic falls through to egsel's choice. Defaults at other
metrics are entirely the admin's business; egsel neither inspects nor
reports them.

## Startup: adopt-or-seed

The slot is kernel state and survives the daemon (crash-safe by
construction: daemon dies, routes stay). On start egsel *reads* the slot
instead of resetting it:

- owned by a roster candidate → adopt as current, cold timers. A
  restart by itself never switches anything.
- owned by something else (set by hand) → hold it, loudly log it, never
  evict what you cannot assess.
- empty → seed within a budget with **priority deferral**: probe
  everyone in parallel, take the top-ranked candidate the moment it
  proves alive; a junior proving alive first waits for its seniors
  until the budget expires (unless they are already proven dead).

A candidate starts **unknown** — not alive, not dead — and acquires a
verdict only from consecutive evidence. No action that depends on a
candidate is taken while it is unknown: *no switching without data*.

Soak timers survive restarts via a small state file (freshness-guarded),
so frequent redeploys do not starve a pending upgrade. The slot owner is
deliberately not stored — the kernel is the source of truth.

## Shared outages: fresh evidence at forced choices

Candidates often share fate — several tunnels ride one physical uplink,
and a provider outage kills them all within a probe round of each
other. Naive rules turn that into two mistakes. During the collapse,
the first-to-die current fails over onto a "still alive" fallback whose
verdict is merely lagging its failing probes — a pointless hop onto a
dying channel. After the outage, whichever candidate's probe happens to
tick first re-seeds the empty slot; a fallback winning that phase
lottery exiles the preferred channel for a whole upgrade window,
turning a seconds-long blip into half an hour on the wrong channel.

Both mistakes come from trusting storm-era verdicts, so forced choices
demand fresh evidence instead:

- A failover or seed target must be *clean*: debounced-alive **and**
  its latest probe succeeded. Alive on an active fail streak is
  mid-collapse, not a refuge.
- Passing over a higher-prio candidate takes *fresh death evidence*: a
  probe lost at or after the moment a clean candidate (re)appeared —
  the signature of an outage ending. A stale Dead verdict from the
  storm gets one probe round to refute itself; a senior that proves
  alive in that round takes the slot by rank with no soak (the move is
  forced, so the upgrade window does not apply).

The wait is bounded by a short grace (derived from the probe timings —
about one probe round plus the up-threshold climb) so a wedged prober
cannot pin the slot, and it never delays the common case: when a
channel dies alone, the very probes that proved it dead are the fresh
evidence, and failover onto a clean fallback is as instant as ever.

## Manual override: `egsel force`

The daemon owns the slot, but the admin outranks it — for a moment.
`egsel force <candidate>` switches at once, unconditionally: no rank,
soak, or freshness demands, dead or unknown targets included — the
explicit command is the evidence. There is no pin: from the next pass
(at most one probe interval) the automatic policy speaks again off the
new state. A dead target is failed over off; a working one holds the
slot like any current — except that the force opens a fresh *trust
episode*: challengers count their upgrade soak from the kick, not from
their own older alive-since, so an already-soaked senior cannot reclaim
the slot two seconds later and every kick buys a full window. The
episode boundary survives restarts via the state file. With several
selectors configured, name one: `egsel force <selector> <candidate>`;
a bare candidate must be unique across rosters.

## When nothing is reachable

An explicit, required policy choice per selector:

- `fail_mode = "open"` — clear the slot; traffic falls through to
  whatever routing remains. Right when the slot is an extra path with a
  sane (if degraded) fallback. No blackhole via a dead route.
- `fail_mode = "closed"` — keep the slot pointed at the best static
  rank even if dead: a route beats no route when the slot is the
  machine's only default (the `table = "main"` multi-WAN case).

## Explainability

A policy this deliberate looks like "why has it not switched?!" from the
outside, so answering that is a first-class duty:

```
$ egsel status
selector freedom (default route in table freedom), fail-open, window 30m0s
  current: wgse2 since 12:04:17
  decision: current wgse2 alive; wgse alive 18m2s of 30m0s window — not yet eligible to preempt
    wgse   prio 100/0    ALIVE  loss 0% rtt 48.2ms      alive 18m2s of 30m0s window; preempts current in 11m58s
    wgse2  prio  50/10   ALIVE  loss 0% rtt 52.1ms      current
    wgie   prio  50/0    DEAD   loss 100%               dead since 12:04:11 (3 probes lost): timeout (1s)
```

`egsel status -json` emits the same as JSON, over a unix control socket.

## Configuration

See [dist/egsel.toml.example](dist/egsel.toml.example) for a commented
config: the exit-tunnel case it was built for, and a multi-WAN sketch.
`egsel check -c <file>` validates without running.

## Connection breakage and flow pinning

Switching a kernel route reroutes existing flows; whether that breaks
connections is a deployment property, not the daemon's. egsel performs
exactly one `ip route replace` and is agnostic to flow pinning (conntrack
marks + per-mark routes). If the admin pins flows, an upgrade drains old
connections gracefully and the soak window is pure trust-building; if
not, the window is what keeps upgrades rare. Only flows of a genuinely
dead channel break — and those are unsalvageable anyway.

## Running

Privileges: route changes need CAP_NET_ADMIN; probing prefers
unprivileged ICMP ping sockets (`net.ipv4.ping_group_range`, open to
everyone under systemd's defaults) and falls back to raw ICMP
(CAP_NET_RAW) where they are disabled. Plain root is simplest; a
hardened unit gets by with `User=` plus
`AmbientCapabilities=CAP_NET_ADMIN`. Build and install:

```
go build -o egsel . && install -m 755 egsel /usr/local/bin/
```

systemd units are in [dist/](dist/): `egsel.service` and
`egsel-candidates.target`, the grouping point the daemon orders after.
Enroll each candidate with

```
systemctl add-wants egsel-candidates.target wg-quick@wgse.service
```

(to unenroll, remove the symlink it created under
`/etc/systemd/system/egsel-candidates.target.wants/` — there is no
`remove-wants`). Target units automatically order `After=` everything
they pull in, so the daemon starts once every enrolled candidate has
settled — no per-instance drop-ins, no names hardwired into the
daemon's unit. A dedicated target is used instead of the stock
`wg-quick.target` on purpose: every `wg-quick@` instance is
`PartOf=wg-quick.target`, making that target a stop/restart hammer for
*all* tunnels on the host, candidates or not. Ordering is only a nicety
for an unbiased first seed — adopt-or-seed converges from any order —
so non-WireGuard candidates can be enrolled with whatever unit their
stack offers (e.g. `sys-subsystem-net-devices-*.device`), or not at
all.

Keep all candidate tunnels up permanently — warm standby makes failover
instant, and each tunnel should carry its own routing table (wg-quick
`Table = <own>`) so they do not fight over the switched one.

## License

Public domain — see [UNLICENSE](UNLICENSE).
