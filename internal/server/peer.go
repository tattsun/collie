package server

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// loopbackPeerUID returns the uid owning the client end of a loopback TCP
// connection, by finding the client's socket in /proc/net/tcp{,6}.
//
// This lets Collie trust `tailscale serve`'s identity headers only on
// connections made by tailscaled (root) — not by any other local process,
// such as an agent that manages to reach the loopback interface.
func loopbackPeerUID(remote, local string) (int, error) {
	rAddr, err := net.ResolveTCPAddr("tcp", remote)
	if err != nil {
		return -1, err
	}
	lAddr, err := net.ResolveTCPAddr("tcp", local)
	if err != nil {
		return -1, err
	}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		uid, err := findSocketUID(f, rAddr, lAddr)
		if err == nil {
			return uid, nil
		}
	}
	return -1, errors.New("peer socket not found")
}

func findSocketUID(path string, client, server *net.TCPAddr) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return -1, err
	}
	defer f.Close()
	v6 := strings.HasSuffix(path, "6")
	wantLocal := procAddr(client, v6)
	wantRemote := procAddr(server, v6)
	if wantLocal == "" || wantRemote == "" {
		return -1, errors.New("address family mismatch")
	}
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 8 {
			continue
		}
		if strings.EqualFold(fields[1], wantLocal) && strings.EqualFold(fields[2], wantRemote) {
			return strconv.Atoi(fields[7])
		}
	}
	return -1, errors.New("not found")
}

// procAddr formats an address the way /proc/net/tcp{,6} does: each 4-byte
// group of the (network-order) IP read as a native-endian integer in hex,
// then ":" and the port in hex.
func procAddr(a *net.TCPAddr, v6 bool) string {
	var ip []byte
	if v6 {
		ip = a.IP.To16()
	} else {
		ip = a.IP.To4()
	}
	if ip == nil {
		return ""
	}
	var sb strings.Builder
	for i := 0; i < len(ip); i += 4 {
		fmt.Fprintf(&sb, "%08X", binary.NativeEndian.Uint32(ip[i:i+4]))
	}
	return fmt.Sprintf("%s:%04X", sb.String(), a.Port)
}
