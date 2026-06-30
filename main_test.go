package main

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// buildResponse parses a packed DNS query and builds a packed response with the
// given RCODE, echoing back the transaction ID and question.
func buildResponse(t *testing.T, query []byte, rcode dnsmessage.RCode) []byte {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(query); err != nil {
		t.Fatalf("server failed to unpack query: %v", err)
	}
	resp := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID:       m.Header.ID,
			Response: true,
			RCode:    rcode,
		},
		Questions: m.Questions,
	}
	packed, err := resp.Pack()
	if err != nil {
		t.Fatalf("server failed to pack response: %v", err)
	}
	return packed
}

// startUDPServer launches a UDP DNS responder that replies with the given RCODE.
func startUDPServer(t *testing.T, rcode dnsmessage.RCode) (string, func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen udp: %v", err)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			resp := buildResponse(t, buf[:n], rcode)
			pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String(), func() { pc.Close() }
}

// startTCPServer launches a TCP DNS responder (2-byte length framing) that
// replies with the given RCODE.
func startTCPServer(t *testing.T, rcode dnsmessage.RCode) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen tcp: %v", err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				lenBuf := make([]byte, 2)
				if _, err := io.ReadFull(c, lenBuf); err != nil {
					return
				}
				query := make([]byte, binary.BigEndian.Uint16(lenBuf))
				if _, err := io.ReadFull(c, query); err != nil {
					return
				}
				resp := buildResponse(t, query, rcode)
				framed := make([]byte, 2+len(resp))
				binary.BigEndian.PutUint16(framed[:2], uint16(len(resp)))
				copy(framed[2:], resp)
				c.Write(framed)
			}(conn)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func TestPerformDNSQuery(t *testing.T) {
	cases := []struct {
		name     string
		protocol string
		rcode    dnsmessage.RCode
		want     bool
	}{
		{"udp success", "udp", dnsmessage.RCodeSuccess, true},
		{"tcp success", "tcp", dnsmessage.RCodeSuccess, true},
		{"udp servfail", "udp", dnsmessage.RCodeServerFailure, false},
		{"tcp nxdomain", "tcp", dnsmessage.RCodeNameError, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var addr string
			var stop func()
			if tc.protocol == "udp" {
				addr, stop = startUDPServer(t, tc.rcode)
			} else {
				addr, stop = startTCPServer(t, tc.rcode)
			}
			defer stop()

			res := performDNSQuery(Config{
				DNSServer:  addr,
				Domain:     "example.com",
				RecordType: 1,
				Protocol:   tc.protocol,
				Timeout:    2 * time.Second,
			})
			if res.Success != tc.want {
				t.Errorf("got Success=%v, want %v", res.Success, tc.want)
			}
		})
	}
}

func TestPerformDNSQueryTimeout(t *testing.T) {
	// A listener that accepts/receives but never replies should time out.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}
	defer pc.Close()

	res := performDNSQuery(Config{
		DNSServer:  pc.LocalAddr().String(),
		Domain:     "example.com",
		RecordType: 1,
		Protocol:   "udp",
		Timeout:    200 * time.Millisecond,
	})
	if res.Success {
		t.Errorf("expected timeout to fail, got success")
	}
}

func TestPercentile(t *testing.T) {
	durations := []time.Duration{
		1 * time.Millisecond,
		2 * time.Millisecond,
		3 * time.Millisecond,
		4 * time.Millisecond,
		5 * time.Millisecond,
	}
	if got := percentile(durations, 0); got != 1 {
		t.Errorf("p0 = %v, want 1", got)
	}
	if got := percentile(durations, 100); got != 5 {
		t.Errorf("p100 = %v, want 5", got)
	}
	if got := percentile(durations, 50); got != 3 {
		t.Errorf("p50 = %v, want 3", got)
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("empty p50 = %v, want 0", got)
	}
}
