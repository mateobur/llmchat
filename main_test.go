package main

import (
	"net"
	"testing"
)

func TestIsLoopback(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:8080": true,
		"127.0.0.53:80":  true,
		"[::1]:8080":     true,
		// ":8080" is what a bare port listens as, and it reaches the network.
		"[::]:8080":      false,
		"0.0.0.0:8080":   false,
		"192.168.1.5:80": false,
		"10.0.0.1:8080":  false,
		"example.com:80": false, // a hostname could resolve anywhere
		"nonsense":       false, // no port at all
	}
	for in, want := range cases {
		if got := isLoopback(fakeAddr(in)); got != want {
			t.Errorf("isLoopback(%q) = %v; want %v", in, got, want)
		}
	}
}

// fakeAddr is a net.Addr that reports exactly the string given.
type fakeAddr string

func (f fakeAddr) Network() string { return "tcp" }
func (f fakeAddr) String() string  { return string(f) }

// Guard the real listener too: a bare port must resolve to something exposed,
// and the flag default must not.
func TestDefaultAddressIsLoopbackAndBarePortIsNot(t *testing.T) {
	for addr, wantLoopback := range map[string]bool{"127.0.0.1:0": true, ":0": false} {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("listening on %q: %v", addr, err)
		}
		if got := isLoopback(ln.Addr()); got != wantLoopback {
			t.Errorf("listening on %q gave %s, isLoopback = %v; want %v",
				addr, ln.Addr(), got, wantLoopback)
		}
		ln.Close()
	}
}
