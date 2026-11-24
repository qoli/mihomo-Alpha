package outboundgroup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/component/smart"
	C "github.com/metacubex/mihomo/constant"
)

func TestSmartConcurrentSelectionSeparatesDestinationFamilies(t *testing.T) {
	smart.InitCache()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deadline, _ := ctx.Deadline()

	const target = "GeoIP [telegram]"
	const ipv6Address = "[2001:b28:f23d:f001::a]:443"
	const ipv6Error = "502 Could not resolve host [2001:b28:f23d:f001::a]"
	ipv4Addresses := []string{"149.154.175.50:5222", "149.154.175.58:5222"}
	seen := make(chan string, 3)
	ipv6Started := make(chan struct{})
	releaseIPv6 := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseIPv6) }) }

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			t.Errorf("proxy method = %s, want CONNECT", r.Method)
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack CONNECT: %v", err)
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(deadline)
		seen <- r.Host
		if r.Host == ipv6Address {
			close(ipv6Started)
			select {
			case <-releaseIPv6:
			case <-ctx.Done():
				return
			}
			_, _ = fmt.Fprintf(rw, "HTTP/1.1 %s\r\nContent-Length: 0\r\n\r\n", ipv6Error)
			_ = rw.Flush()
			return
		}
		_, _ = io.WriteString(rw, "HTTP/1.1 200 Connection established\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw)
	}))
	t.Cleanup(func() {
		release()
		server.Close()
	})
	addr := server.Listener.Addr().(*net.TCPAddr)
	httpProxy, err := outbound.NewHttp(outbound.HttpOption{Name: "loopback", Server: addr.IP.String(), Port: addr.Port})
	if err != nil {
		t.Fatal(err)
	}
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
	s.providerProxies = []C.Proxy{adapter.NewProxy(httpProxy)}
	t.Cleanup(func() { s.store.DeleteUnwrapResult(s.Name(), s.configName, target, "", nil) })

	dial := func(address string) error {
		dst := netip.MustParseAddrPort(address)
		metadata := &C.Metadata{NetWork: C.TCP, DstIP: dst.Addr(), DstPort: dst.Port(), SmartTarget: target}
		conn, err := s.DialContext(ctx, metadata)
		if err != nil {
			return err
		}
		defer conn.Close()
		_ = conn.SetDeadline(deadline)
		if _, err := io.WriteString(conn, address); err != nil {
			return err
		}
		got := make([]byte, len(address))
		if _, err := io.ReadFull(conn, got); err != nil {
			return err
		}
		if string(got) != address {
			return fmt.Errorf("proxy echo = %q, want %q", got, address)
		}
		return nil
	}
	ipv6Result := make(chan error, 1)
	go func() { ipv6Result <- dial(ipv6Address) }()
	select {
	case <-ipv6Started:
	case <-ctx.Done():
		t.Fatal("IPv6 CONNECT did not start")
	}

	ipv4Result := make(chan error, 1)
	go func() { ipv4Result <- dial(ipv4Addresses[0]) }()
	// On the regression, IPv4 cannot reach the proxy until the IPv6 election
	// ends. Release that election after the guard so its original 502 is visible.
	guard := time.NewTimer(C.DefaultTCPTimeout / 2)
	defer guard.Stop()
	completed := false
	var ipv4Err error
	select {
	case ipv4Err = <-ipv4Result:
		completed = true
	case <-guard.C:
	case <-ctx.Done():
		t.Fatal("IPv4 dial did not finish")
	}
	if completed && ipv4Err == nil {
		// Reusing the same-family winner must still dial the second IPv4's own
		// destination while the IPv6 leader remains pending.
		if err := dial(ipv4Addresses[1]); err != nil {
			t.Errorf("second IPv4 dial: %v", err)
		}
	}
	release()
	select {
	case err := <-ipv6Result:
		if err == nil || err.Error() != ipv6Error {
			t.Errorf("IPv6 dial = %v, want %s", err, ipv6Error)
		}
	case <-ctx.Done():
		t.Fatal("IPv6 dial did not finish after release")
	}
	if !completed {
		select {
		case ipv4Err = <-ipv4Result:
		case <-ctx.Done():
			t.Fatal("IPv4 dial did not finish after IPv6 failure")
		}
		t.Error("IPv4 waited for the IPv6 election instead of completing independently")
	}
	if ipv4Err != nil {
		t.Errorf("IPv4 dial = %v, want independent success", ipv4Err)
	}
	counts := make(map[string]int)
	for len(seen) > 0 {
		counts[<-seen]++
	}
	for _, address := range append(ipv4Addresses, ipv6Address) {
		if counts[address] != 1 {
			t.Errorf("proxy CONNECT count for %s = %d, want 1 (all: %v)", address, counts[address], counts)
		}
	}
}

func TestSmartConcurrentSelectionDestinationScope(t *testing.T) {
	smart.InitCache()
	ipv4 := C.Metadata{DstIP: netip.MustParseAddr("149.154.175.50")}
	ipv4Other := C.Metadata{DstIP: netip.MustParseAddr("149.154.175.58")}
	mapped := C.Metadata{DstIP: netip.MustParseAddr("::ffff:149.154.175.58")}
	ipv6 := C.Metadata{DstIP: netip.MustParseAddr("2001:b28:f23d:f001::a")}
	ipv6Other := C.Metadata{DstIP: netip.MustParseAddr("2001:b28:f23f:f005::a")}
	domain := C.Metadata{Host: "api.telegram.org"}
	domainIPv4 := domain
	domainIPv4.DstIP = ipv4.DstIP
	domainIPv6 := domain
	domainIPv6.DstIP = ipv6.DstIP
	mapping := domainIPv4
	mapping.DNSMode = C.DNSMapping
	hosts := domainIPv6
	hosts.DNSMode = C.DNSHosts

	for _, network := range []C.NetWork{C.TCP, C.UDP} {
		for _, tc := range []struct {
			name   string
			first  C.Metadata
			second C.Metadata
			share  bool
		}{
			{name: "different IPv4 destinations", first: ipv4, second: ipv4Other, share: true},
			{name: "mapped IPv4", first: ipv4, second: mapped, share: true},
			{name: "different IPv6 destinations", first: ipv6, second: ipv6Other, share: true},
			{name: "IPv4 and IPv6", first: ipv6, second: ipv4},
			{name: "mapped IPv4 and IPv6", first: ipv6, second: mapped},
			{name: "hostnames", first: domain, second: C.Metadata{Host: "web.telegram.org"}, share: true},
			{name: "host with resolved IPv4", first: domain, second: domainIPv4, share: true},
			{name: "host with resolved IPv6", first: domainIPv4, second: domainIPv6, share: true},
			{name: "hostname and IPv4", first: domainIPv4, second: ipv4},
			{name: "hostname and IPv6", first: domain, second: ipv6},
			{name: "DNSMapping pure IPv4", first: *mapping.Pure(), second: ipv4, share: true},
			{name: "DNSHosts pure IPv6", first: *hosts.Pure(), second: ipv6, share: true},
		} {
			t.Run(network.String()+"/"+tc.name, func(t *testing.T) {
				s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
				s.providerProxies = []C.Proxy{smartVideoTestProxy{name: "node", udp: true}}
				first, second := tc.first, tc.second
				first.NetWork, second.NetWork = network, network
				first.SmartTarget, second.SmartTarget = "GeoIP [telegram]", "GeoIP [telegram]"
				first.DstPort, second.DstPort = 443, 5222
				first.SrcIP, second.SrcIP = netip.MustParseAddr("192.0.2.1"), netip.MustParseAddr("2001:db8::1")
				first.DstIPASN, second.DstIPASN = "64500 first", "64501 second"
				leader, err := s.prepareDialSelection(context.Background(), &first)
				if err != nil || leader.flight == nil {
					t.Fatalf("first selection = %+v, %v, want leader", leader, err)
				}
				wantErr := errors.New("leader dial failed")
				finishLeader := func() { s.finishDialSelection(context.Background(), leader, "", wantErr) }
				t.Cleanup(finishLeader)
				waited := false
				waitCtx := smartSelectionWaitContext{Context: context.Background(), onWait: func() {
					waited = true
					finishLeader()
				}}
				selection, err := s.prepareDialSelection(waitCtx, &second)
				if tc.share {
					if !waited || !errors.Is(err, wantErr) || selection.flight != nil {
						t.Fatalf("same-scope selection = %+v, %v (waited: %t), want leader failure", selection, err, waited)
					}
				} else {
					if waited || err != nil || selection.flight == nil || selection.flight == leader.flight {
						t.Fatalf("different-scope selection = %+v, %v (waited: %t), want independent leader", selection, err, waited)
					}
					s.finishDialSelection(context.Background(), selection, "", wantErr)
				}
			})
		}
	}
}
