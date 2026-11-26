package server

import (
	"net"
	"os"
	"testing"
)

func TestLoopbackPeerUID(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		addr := "127.0.0.1:0"
		if network == "tcp6" {
			addr = "[::1]:0"
		}
		ln, err := net.Listen(network, addr)
		if err != nil {
			t.Logf("%s: %v (skipped)", network, err)
			continue
		}
		accepted := make(chan net.Conn, 1)
		go func() { c, _ := ln.Accept(); accepted <- c }()
		c, err := net.Dial(network, ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		srv := <-accepted
		// From the server's point of view: remote = client, local = listener.
		uid, err := loopbackPeerUID(srv.RemoteAddr().String(), srv.LocalAddr().String())
		if err != nil {
			t.Fatalf("%s: %v", network, err)
		}
		if uid != os.Getuid() {
			t.Fatalf("%s: uid = %d, want %d", network, uid, os.Getuid())
		}
		c.Close()
		srv.Close()
		ln.Close()
	}
}
