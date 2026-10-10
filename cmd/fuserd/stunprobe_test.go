package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"fuseronline/internal/stunserver"
)

func TestStunProbeCommand(t *testing.T) {
	s, err := stunserver.Listen(stunserver.Options{Bind: "127.0.0.1", Ports: []int{0, 0}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a := s.Addrs()
	var out bytes.Buffer
	err = stunProbeCommand([]string{"-alt-port", strconv.Itoa(a[1].Port), a[0].String()}, &out)
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"local socket", "port " + strconv.Itoa(a[0].Port), "port " + strconv.Itoa(a[1].Port),
		"mapping        none"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}

	// Nothing answering: a port taken from the closed server.
	s.Close()
	out.Reset()
	err = stunProbeCommand([]string{"-alt-port", "0", "-wait", "100ms", a[0].String()}, &out)
	if err == nil || !strings.Contains(out.String(), "no STUN answer") {
		t.Fatalf("closed server: %v\n%s", err, out.String())
	}
	if err := stunProbeCommand(nil, &out); err == nil {
		t.Fatal("no target accepted")
	}
}
