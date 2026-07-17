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
it dies traffic falls through to egsel's choice. A lower-metric foreign
default shadowing ours is surfaced in `egsel status` and the log rather
than silently turning the daemon into a no-op.

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

systemd units in [dist/](dist/): `egsel.service` plus a
`wireguard-tunnels.target` pattern to group candidate tunnels without
hardwiring their names into the daemon's unit (systemd dependencies take
no wildcards). Keep all candidate tunnels up permanently — warm standby
makes failover instant, and each tunnel should carry its own routing
table (wg-quick `Table = <own>`) so they do not fight over the switched
one.

## License

Public domain — see [UNLICENSE](UNLICENSE).
