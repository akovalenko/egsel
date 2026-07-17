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
}

// Run probes on the interval until ctx is done, feeding results to out.
// The socket is opened per probe: a recreated interface (e.g. after a
// wg-quick restart) changes ifindex, and a fresh SO_BINDTODEVICE picks
// it up.
func (p *Prober) Run(ctx context.Context, out chan<- core.ProbeResult) {
	id := echoID(p.Cand)
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
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.IPPROTO_ICMP)
	if err != nil {
		return 0, fmt.Errorf("raw socket: %w", err)
	}
	defer unix.Close(fd)
	if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, p.Cand); err != nil {
		return 0, fmt.Errorf("bind to %s: %w", p.Cand, err)
	}

	dst := &unix.SockaddrInet4{Addr: p.Peer.As4()}
	start := time.Now()
	deadline := start.Add(p.Timeout)
	if err := unix.Sendto(fd, echoRequest(id, seq), 0, dst); err != nil {
		return 0, fmt.Errorf("send: %w", err)
	}

	buf := make([]byte, 1500)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return 0, fmt.Errorf("timeout (%s)", p.Timeout)
		}
		tv := unix.NsecToTimeval(left.Nanoseconds())
		if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			return 0, fmt.Errorf("rcvtimeo: %w", err)
		}
		n, _, err := unix.Recvfrom(fd, buf, 0)
		switch {
		case err == unix.EAGAIN || err == unix.EWOULDBLOCK:
			return 0, fmt.Errorf("timeout (%s)", p.Timeout)
		case err == unix.EINTR:
			continue
		case err != nil:
			return 0, fmt.Errorf("recv: %w", err)
		}
		if isOurEchoReply(buf[:n], id, seq) {
			return time.Since(start), nil
		}
		// unrelated ICMP on the device; keep waiting
	}
}

// echoID derives a stable 16-bit echo identifier from the pid and the
// interface name, so parallel probers on one host tell replies apart
// (the reply filter matches both id and seq anyway).
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

// isOurEchoReply parses a raw IPv4 packet (raw ICMP sockets deliver the
// IP header) and matches an echo reply against our id and seq.
func isOurEchoReply(pkt []byte, id, seq int) bool {
	if len(pkt) < 20 {
		return false
	}
	hlen := int(pkt[0]&0x0f) * 4
	if len(pkt) < hlen+8 {
		return false
	}
	icmp := pkt[hlen:]
	return icmp[0] == 0 && icmp[1] == 0 && // echo reply
		binary.BigEndian.Uint16(icmp[4:]) == uint16(id) &&
		binary.BigEndian.Uint16(icmp[6:]) == uint16(seq)
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
