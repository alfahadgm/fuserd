package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"fuseronline/internal/stunserver"
)

const stunProbeUsage = `usage:
  fuserd stun-probe [-alt-port 3479] <host>[:port]

Sends STUN Binding requests from one local UDP socket to fuserd's STUN port (default 3478) and its alternate port,
and prints the public endpoint each saw and what that says about this network's NAT. Run it from a player's
network to check that the server's STUN ports are reachable. -alt-port 0 skips the second port.`

// stunProbeCommand is "fuserd stun-probe ...".
func stunProbeCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("stun-probe", flag.ContinueOnError)
	fs.SetOutput(out)
	altPort := fs.Int("alt-port", 3479, "the alternate STUN port (0: none)")
	wait := fs.Duration("wait", time.Second, "how long to wait for each answer")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New(stunProbeUsage)
	}
	target := fs.Arg(0)
	if _, _, err := net.SplitHostPort(target); err != nil {
		target = net.JoinHostPort(target, "3478")
	}
	primary, err := net.ResolveUDPAddr("udp4", target)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	// The local address the OS routes toward the server, for telling "no NAT" apart.
	local := conn.LocalAddr().(*net.UDPAddr)
	if c, err := net.Dial("udp4", primary.String()); err == nil {
		local = &net.UDPAddr{IP: c.LocalAddr().(*net.UDPAddr).IP, Port: local.Port}
		c.Close()
	}
	fmt.Fprintf(out, "local socket   %s\n", local)

	probe := func(addr *net.UDPAddr) *net.UDPAddr {
		mapped, err := stunserver.Probe(conn, addr, 3, *wait)
		if err != nil {
			fmt.Fprintf(out, "%-14s %s: %v (UDP blocked, wrong host, or the port is closed)\n", "port "+strconv.Itoa(addr.Port), addr, err)
			return nil
		}
		fmt.Fprintf(out, "%-14s %s sees %s\n", "port "+strconv.Itoa(addr.Port), addr, mapped)
		return mapped
	}
	a := probe(primary)
	var b *net.UDPAddr
	if *altPort != 0 {
		b = probe(&net.UDPAddr{IP: primary.IP, Port: *altPort})
	}
	if a == nil && b == nil {
		return errors.New("no STUN answer from either port")
	}
	m := stunserver.Classify(local, a, b)
	fmt.Fprintf(out, "mapping        %s - %s\n", m, mappingAdvice(m))
	return nil
}

func mappingAdvice(m stunserver.Mapping) string {
	switch m {
	case stunserver.MappingNone:
		return "this machine has a public address; it can host without traversal"
	case stunserver.MappingIndependent:
		return "the same public endpoint toward both ports; hole punching should work"
	case stunserver.MappingDependent:
		return "a different public endpoint per destination (symmetric NAT); hole punching is unlikely, a relay would be needed"
	default:
		return "only one port answered, so the mapping can't be classified"
	}
}
