package server

import (
	"fmt"
	"net"
	"strings"
	"testing"
)

func TestListenFallback(t *testing.T) {
	// Reserve a consecutive pair, retrying ephemeral allocation if necessary.
	var first, second net.Listener
	for attempt := 0; attempt < 100; attempt++ {
		var err error
		first, err = Listen("127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := first.Addr().(*net.TCPAddr).Port
		if port == 65535 {
			first.Close()
			continue
		}
		second, err = Listen(fmt.Sprintf("127.0.0.1:%d", port+1))
		if err == nil {
			break
		}
		first.Close()
	}
	if second == nil {
		t.Fatal("cannot reserve consecutive test ports")
	}
	defer first.Close()
	defer second.Close()
	addr := first.Addr().String()
	if ln, err := ListenFallback(addr, 0, nil); err == nil {
		ln.Close()
		t.Fatal("fallback=0 accepted occupied port")
	}
	if ln, err := ListenFallback(addr, 1, nil); err == nil {
		ln.Close()
		t.Fatal("all busy succeeded")
	} else if !strings.Contains(err.Error(), "all in use") {
		t.Fatal(err)
	}
	want := second.Addr().String()
	second.Close()
	ln, err := ListenFallback(addr, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ln.Addr().String() != want {
		t.Fatalf("addr=%s want %s", ln.Addr(), want)
	}
}
