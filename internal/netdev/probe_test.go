package netdev

import "testing"

func TestEchoRoundtripRaw(t *testing.T) {
	req := echoRequest(0x1234, 7)
	if checksum(req) != 0 {
		t.Fatal("checksum over a checksummed message must be zero")
	}
	// synthesize the reply as a raw socket would deliver it: IP header +
	// ICMP with type flipped to echo-reply and checksum fixed up
	reply := append(make([]byte, 20), req...)
	reply[0] = 0x45
	icmp := reply[20:]
	icmp[0] = 0
	icmp[2], icmp[3] = 0, 0
	sum := checksum(icmp)
	icmp[2], icmp[3] = byte(sum>>8), byte(sum)

	msg := stripIPHeader(reply)
	if msg == nil {
		t.Fatal("must strip the IP header")
	}
	if got := matchEchoReply(msg, 0x1234, 7, true); got != true {
		t.Fatal("must match our reply")
	}
	if matchEchoReply(msg, 0x1234, 8, true) {
		t.Fatal("wrong seq must not match")
	}
	if matchEchoReply(msg, 0x4321, 7, true) {
		t.Fatal("wrong id must not match on raw")
	}
	if stripIPHeader(reply[:15]) != nil {
		t.Fatal("short packet must strip to nil")
	}
}

func TestEchoRoundtripPingSocket(t *testing.T) {
	// a ping socket delivers bare ICMP with a kernel-rewritten id: no
	// IP header to strip, id ignored, seq still checked
	reply := echoRequest(0x9999, 3) // arbitrary id, as the kernel would rewrite it
	reply[0] = 0
	if got := matchEchoReply(reply, 0x1234, 3, false); got != true {
		t.Fatal("ping-socket reply must match regardless of id")
	}
	if matchEchoReply(reply, 0x1234, 4, false) {
		t.Fatal("wrong seq must not match")
	}
}
