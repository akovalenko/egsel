// Package netdev is the network-device transport: candidates are plain
// L3 interfaces (WireGuard tunnels, ethernet uplinks — nothing here is
// tunnel-specific). It probes each one the way traffic will use it
// (ICMP echo to a per-candidate address with the socket bound to the
// device) and owns the default route of a policy table through ip(8).
package netdev

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"net/netip"
	"os"
	"time"

	"golang.org/x/sys/unix"

	"github.com/akovalenko/egsel/internal/core"
)

// Prober measures one candidate end-to-end over the pair (protocol,
// path) that will carry traffic. For a WireGuard tunnel an echo to the
// far tunnel address checks the crypto, the underlay beneath it and the
// peer in one probe; for an ethernet uplink an echo to the gateway (or
// beyond) plays the same role. The SO_BINDTODEVICE binding makes it
// config-independent — a broken policy routing setup cannot masquerade
// as a dead channel.
type Prober struct {
	Cand     string // candidate name (= interface name)
	Peer     netip.Addr
	Interval time.Duration
	Timeout  time.Duration

	// The probe socket persists across probes: on the ping-socket path
	// the kernel assigns the echo id per socket, so a per-probe socket
	// would rotate the id every 2s and leak a fresh conntrack/NAT entry
	// per probe on every stateful box along the path. One long-lived
	// socket = one id = one entry, refreshed like a long-running ping.
	fd  int
	raw bool
}

// Run probes on the interval until ctx is done, feeding results to out.
func (p *Prober) Run(ctx context.Context, out chan<- core.ProbeResult) {
	id := echoID(p.Cand)
	p.fd = -1
	defer p.closeSocket()
	tick := time.NewTicker(p.Interval)
	defer tick.Stop()
	seq := 0
	for {
		seq++
		rtt, err := p.probeOnce(id, seq&0xffff)
		r := core.ProbeResult{Cand: p.Cand, OK: err == nil, RTT: rtt, At: time.Now()}
		if err != nil {
			r.Err = err.Error()
		}
		select {
		case out <- r:
		case <-ctx.Done():
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}

func (p *Prober) probeOnce(id, seq int) (time.Duration, error) {
	if p.fd < 0 {
		fd, raw, err := icmpSocket()
		if err != nil {
			return 0, err
		}
		p.fd, p.raw = fd, raw
	}
	// Re-arm the device binding every probe: SO_BINDTODEVICE resolves
	// the name to an ifindex at setsockopt time, so a recreated
	// interface (e.g. after a wg-quick restart) is picked up on the
	// next probe without giving up the socket — and with it the echo
	// id. Failure (ENODEV while the interface is gone) keeps the
	// socket: nothing about it went bad.
	if err := unix.SetsockoptString(p.fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, p.Cand); err != nil {
		return 0, fmt.Errorf("bind to %s: %w", p.Cand, err)
	}

	dst := &unix.SockaddrInet4{Addr: p.Peer.As4()}
	start := time.Now()
	deadline := start.Add(p.Timeout)
	if err := unix.Sendto(p.fd, echoRequest(id, seq), 0, dst); err != nil {
		p.closeSocket()
		return 0, fmt.Errorf("send: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return 0, fmt.Errorf("timeout (%s)", p.Timeout)
		}
		tv := unix.NsecToTimeval(left.Nanoseconds())
		if err := unix.SetsockoptTimeval(p.fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			p.closeSocket()
			return 0, fmt.Errorf("rcvtimeo: %w", err)
		}
		n, _, err := unix.Recvfrom(p.fd, buf, 0)
		switch {
		case err == unix.EAGAIN || err == unix.EWOULDBLOCK:
			return 0, fmt.Errorf("timeout (%s)", p.Timeout)
		case err == unix.EINTR:
			continue
		case err != nil:
			p.closeSocket()
			return 0, fmt.Errorf("recv: %w", err)
		}
		msg := buf[:n]
		if p.raw {
			msg = stripIPHeader(msg)
		}
		// A ping socket receives only its own replies (the kernel
		// demultiplexes by the id it assigned on send), so the id check
		// applies to raw sockets only. Late replies to earlier probes
		// fail the seq check and are skipped.
		if matchEchoReply(msg, id, seq, p.raw) {
			return time.Since(start), nil
		}
		// unrelated ICMP on the device; keep waiting
	}
}

// closeSocket drops the persistent socket after a hard error; the next
// probe reopens fresh. Timeouts and bind failures do not come here —
// the socket itself is healthy in both cases.
func (p *Prober) closeSocket() {
	if p.fd >= 0 {
		unix.Close(p.fd)
		p.fd = -1
	}
}

// icmpSocket prefers an unprivileged ICMP "ping socket" (SOCK_DGRAM,
// available to gids within net.ipv4.ping_group_range — open to all
// under systemd's defaults) and falls back to a raw socket
// (CAP_NET_RAW) where ping sockets are disabled.
func icmpSocket() (fd int, raw bool, err error) {
	fd, err = unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err == nil {
		return fd, false, nil
	}
	fd, err = unix.Socket(unix.AF_INET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err != nil {
		return -1, false, fmt.Errorf("icmp socket (ping and raw): %w", err)
	}
	return fd, true, nil
}

// echoID derives a stable 16-bit echo identifier from the pid and the
// interface name, so parallel probers on one host tell replies apart
// on raw sockets (ping sockets ignore it: the kernel assigns its own).
func echoID(iface string) int {
	h := fnv.New32a()
	h.Write([]byte(iface))
	return int((uint32(os.Getpid()) ^ h.Sum32()) & 0xffff)
}

// echoRequest builds an ICMP echo request; 8-byte header, no payload
// beyond a marker. Hand-rolled: the whole protocol here is two message
// types, not worth a dependency.
func echoRequest(id, seq int) []byte {
	msg := make([]byte, 8+8)
	msg[0] = 8 // echo request
	binary.BigEndian.PutUint16(msg[4:], uint16(id))
	binary.BigEndian.PutUint16(msg[6:], uint16(seq))
	copy(msg[8:], "egsel-pb")
	binary.BigEndian.PutUint16(msg[2:], checksum(msg))
	return msg
}

// stripIPHeader unwraps the IPv4 header raw ICMP sockets deliver; nil
// when the packet is too short to hold one.
func stripIPHeader(pkt []byte) []byte {
	if len(pkt) < 20 {
		return nil
	}
	if hlen := int(pkt[0]&0x0f) * 4; len(pkt) >= hlen {
		return pkt[hlen:]
	}
	return nil
}

// matchEchoReply reports whether msg is an ICMP echo reply carrying our
// seq — and our id, when checkID (raw sockets see everyone's ICMP;
// ping sockets receive only their own, with a kernel-assigned id).
func matchEchoReply(msg []byte, id, seq int, checkID bool) bool {
	if len(msg) < 8 || msg[0] != 0 || msg[1] != 0 {
		return false
	}
	if checkID && binary.BigEndian.Uint16(msg[4:]) != uint16(id) {
		return false
	}
	return binary.BigEndian.Uint16(msg[6:]) == uint16(seq)
}

// checksum is the RFC 1071 ones-complement sum over the message with
// the checksum field zeroed.
func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum > 0xffff {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
