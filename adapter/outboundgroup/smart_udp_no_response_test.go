package outboundgroup

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/component/smart"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

func smartNoResponseTestProxy(t *testing.T, name string) C.Proxy {
	t.Helper()
	return smartPacketDialTestProxy{
		smartVideoTestProxy: smartVideoTestProxy{name: name, udp: true},
		listen: func(context.Context) (C.PacketConn, error) {
			socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				return nil, err
			}
			t.Cleanup(func() { _ = socket.Close() })
			return outbound.NewPacketConn(socket, outbound.NewBase(outbound.BaseOption{Name: name, Type: C.Socks5, UDP: true})), nil
		},
	}
}

func TestSmartUDPNoResponseDeniesNextExitWithoutClosingFlow(t *testing.T) {
	for _, host := range []string{"www.youtube.com", "rr1---sn-test.googlevideo.com"} {
		t.Run(host, func(t *testing.T) {
			smart.InitCache()
			s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart,
				EmptyFallback: smartVideoTestProxy{name: "REJECT"}}), store: &smart.Store{}}
			silent := smartNoResponseTestProxy(t, "silent")
			working := smartNoResponseTestProxy(t, "working")
			s.providerProxies = []C.Proxy{silent, working}
			metadata := &C.Metadata{NetWork: C.UDP, Type: C.TUN, Host: host, DstPort: 443, SmartTarget: "GeoSite [youtube]"}
			raw, err := s.ListenPacketContext(context.Background(), metadata)
			if err != nil {
				t.Fatal(err)
			}
			tracker := statistic.NewUDPTracker(raw, statistic.DefaultManager, metadata, nil, 0, 0, true)
			t.Cleanup(func() { _ = tracker.Close() })
			cacheTarget, cacheASN := smartWinnerCacheScope(metadata, "")
			t.Cleanup(func() { s.store.DeleteUnwrapResult(s.Name(), s.configName, cacheTarget, cacheASN, nil) })
			if got := tracker.Chains()[0]; got != silent.Name() {
				t.Fatalf("initial UDP exit = %q, want %q", got, silent.Name())
			}
			tcp := *metadata
			tcp.NetWork, tcp.UUID = C.TCP, ""
			s.storeDialWinner(&tcp, "", silent, nil)
			tcpTarget, tcpASN := smartWinnerCacheScope(&tcp, "")
			t.Cleanup(func() { s.store.DeleteUnwrapResult(s.Name(), s.configName, tcpTarget, tcpASN, nil) })
			tcpWinner, _ := s.store.GetUnwrapResult(s.Name(), s.configName, tcpTarget, tcpASN)

			server, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = server.Close() })
			_ = server.SetDeadline(time.Now().Add(time.Second))
			if _, err := tracker.WriteTo(make([]byte, smartMonitorMinUploadBytes), server.LocalAddr()); err != nil {
				t.Fatal(err)
			}
			packet := make([]byte, 1024)
			_, peer, err := server.ReadFrom(packet)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			s.sampleActiveConnections(start)
			s.sampleActiveConnections(start.Add(smartMonitorNoResponseAfter - smartMonitorInterval))
			if s.activeNodeCoolingDown(silent.Name(), metadata.WildcardTarget, C.UDP, start) {
				t.Fatal("UDP exit was denied before the no-response window")
			}
			s.sampleActiveConnections(start.Add(smartMonitorNoResponseAfter))
			if !s.activeNodeCoolingDown(silent.Name(), metadata.WildcardTarget, C.UDP, start.Add(smartMonitorNoResponseAfter)) {
				t.Fatal("unanswered UDP exit was not denied")
			}
			if statistic.DefaultManager.Get(tracker.ID()) == nil || tracker.Info().ClosedForDegradation() {
				t.Fatal("UDP veto closed the existing association")
			}
			if got, _ := s.store.GetUnwrapResult(s.Name(), s.configName, tcpTarget, tcpASN); got != tcpWinner {
				t.Fatalf("UDP veto changed the existing TCP winner: %v", got)
			}
			data, err := s.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			var evidence struct {
				UDP   []smartUDPNoResponse    `json:"udpNoResponses"`
				Video []smartVideoDegradation `json:"videoDegradations"`
			}
			if err := json.Unmarshal(data, &evidence); err != nil || len(evidence.UDP) != 1 || evidence.UDP[0].Node != silent.Name() || evidence.UDP[0].Wildcard != metadata.WildcardTarget || len(evidence.Video) != 0 {
				t.Fatalf("UDP veto evidence was missing or leaked into video qualification: %s, %v", data, err)
			}

			// The original association can still deliver a late response.
			if _, err := server.WriteTo([]byte("late response"), peer); err != nil {
				t.Fatal(err)
			}
			_ = tracker.SetReadDeadline(time.Now().Add(time.Second))
			if n, _, err := tracker.ReadFrom(packet); err != nil || string(packet[:n]) != "late response" {
				t.Fatalf("existing association lost its late response: %q, %v", packet[:n], err)
			}

			// A healthy TCP winner is not invalidated by UDP-only evidence.
			selection, err := s.prepareDialSelection(context.Background(), &tcp)
			if err != nil || selection.flight != nil || len(selection.proxies) != 1 || selection.proxies[0].Name() != silent.Name() {
				t.Fatalf("UDP veto affected TCP: %+v, %v", selection, err)
			}

			nextMetadata := *metadata
			nextMetadata.UUID = ""
			next, err := s.ListenPacketContext(context.Background(), &nextMetadata)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = next.Close() })
			if got := next.Chains()[0]; got != working.Name() {
				t.Fatalf("UDP reconnected to %q after veto, want %q", got, working.Name())
			}
			// A late election cannot overwrite the replacement with the denied node.
			s.storeDialWinner(metadata, "", silent, nil)
			if got, _ := s.store.GetUnwrapResult(s.Name(), s.configName, cacheTarget, cacheASN); got == nil || got.Proxy != working.Name() {
				t.Fatalf("denied UDP exit repopulated winner cache: %v", got)
			}

			// Exhaustion must not relax the veto and immediately retry the old exit.
			s.providerProxies = []C.Proxy{silent}
			if got := s.Unwrap(&nextMetadata, false); got != nil {
				t.Fatalf("candidate exhaustion readmitted denied exit: %v", got)
			}
			if pc, err := s.ListenPacketContext(context.Background(), &nextMetadata); err == nil || pc != nil {
				t.Fatalf("candidate exhaustion accepted denied exit: %v, %v", pc, err)
			}
			// The denial is local to this wildcard and respects a manual pin.
			other := *metadata
			other.Host, other.SmartTarget, other.UUID = "api.github.com", "GeoSite [github]", ""
			otherConn, err := s.ListenPacketContext(context.Background(), &other)
			if err != nil {
				t.Fatalf("UDP veto leaked to another target: %v", err)
			}
			t.Cleanup(func() { _ = otherConn.Close() })
			otherTarget, otherASN := smartWinnerCacheScope(&other, "")
			t.Cleanup(func() { s.store.DeleteUnwrapResult(s.Name(), s.configName, otherTarget, otherASN, nil) })
			s.ForceSet(silent.Name())
			pinned, err := s.ListenPacketContext(context.Background(), &nextMetadata)
			if err != nil || pinned.Chains()[0] != silent.Name() {
				t.Fatalf("UDP veto overrode manual selection: %v", err)
			}
			t.Cleanup(func() { _ = pinned.Close() })
			s.ForceSet("")

			afterCooldown := start.Add(smartMonitorNoResponseAfter + smartMonitorCooldown)
			if s.activeNodeCoolingDown(silent.Name(), metadata.WildcardTarget, C.UDP, afterCooldown) {
				t.Fatal("UDP veto outlived its cooldown")
			}
			if got := s.udpNoResponses(afterCooldown, s.GetProxies(false)); len(got) != 0 {
				t.Fatalf("expired UDP veto remained published: %v", got)
			}
			recovered, err := s.ListenPacketContext(context.Background(), &nextMetadata)
			if err != nil || recovered.Chains()[0] != silent.Name() {
				t.Fatalf("UDP exit did not become eligible after cooldown: %v", err)
			}
			t.Cleanup(func() { _ = recovered.Close() })
		})
	}
}

func TestSmartUDPNoResponseKeepsManualAndInternalTraffic(t *testing.T) {
	for _, tc := range []struct {
		name string
		pin  bool
		kind C.Type
		port uint16
	}{
		{name: "manual", pin: true, kind: C.TUN, port: 443},
		{name: "internal", kind: C.INNER, port: 443},
		{name: "other port", kind: C.TUN, port: 53},
	} {
		t.Run(tc.name, func(t *testing.T) {
			smart.InitCache()
			s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
			proxy := smartVideoTestProxy{name: "node", udp: true}
			if tc.pin {
				s.ForceSet(proxy.Name())
			}
			metadata := &C.Metadata{NetWork: C.UDP, Type: tc.kind, DstPort: tc.port, WildcardTarget: "*.youtube.com", SmartTarget: "youtube"}
			pc := &smartMonitorTestPacketConn{}
			tracker := statistic.NewUDPTracker(s.monitorActivePacketConnection(pc, proxy, metadata), statistic.DefaultManager, metadata, nil, 0, 0, true)
			t.Cleanup(func() { _ = tracker.Close() })
			tracker.Info().UploadTotal.Store(6714)
			start := time.Now()
			s.sampleActiveConnections(start)
			s.sampleActiveConnections(start.Add(smartMonitorNoResponseAfter))
			if pc.closed || s.activeNodeCoolingDown(proxy.Name(), metadata.WildcardTarget, C.UDP, start.Add(smartMonitorNoResponseAfter)) {
				t.Fatal("UDP veto changed protected traffic")
			}
		})
	}
}

func TestSmartUDPNoResponseRejectsExitDeniedDuringDial(t *testing.T) {
	smart.InitCache()
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
	started := make(chan struct{})
	release := make(chan struct{})
	pending := &smartMonitorTestPacketConn{}
	silent := smartPacketDialTestProxy{
		smartVideoTestProxy: smartVideoTestProxy{name: "silent", udp: true},
		listen: func(ctx context.Context) (C.PacketConn, error) {
			close(started)
			select {
			case <-release:
				return pending, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		},
	}
	working := smartNoResponseTestProxy(t, "working")
	s.providerProxies = []C.Proxy{silent, working}
	metadata := &C.Metadata{NetWork: C.UDP, Type: C.TUN, Host: "www.youtube.com", DstPort: 443, SmartTarget: "youtube"}
	s.selectProxies(metadata, s.GetProxies(false))
	s.storeDialWinner(metadata, "", silent, nil)
	cacheTarget, cacheASN := smartWinnerCacheScope(metadata, "")
	t.Cleanup(func() { s.store.DeleteUnwrapResult(s.Name(), s.configName, cacheTarget, cacheASN, nil) })
	old := statistic.NewUDPTracker(s.monitorActivePacketConnection(&smartMonitorTestPacketConn{}, silent, metadata), statistic.DefaultManager, metadata, nil, 0, 0, true)
	t.Cleanup(func() { _ = old.Close() })
	old.Info().UploadTotal.Store(6714)
	nextMetadata := *metadata
	nextMetadata.UUID = ""
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		pc, err := s.ListenPacketContext(ctx, &nextMetadata)
		if pc != nil {
			_ = pc.Close()
		}
		result <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("cached UDP dial did not start")
	}
	start := time.Now()
	s.sampleActiveConnections(start)
	s.sampleActiveConnections(start.Add(smartMonitorNoResponseAfter))
	close(release)
	if err := <-result; err == nil || !strings.Contains(err.Error(), "cooling down") || !pending.closed {
		t.Fatalf("in-flight dial returned a newly denied exit: %v, closed=%t", err, pending.closed)
	}
	pc, err := s.ListenPacketContext(context.Background(), &nextMetadata)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if got := pc.Chains()[0]; got != working.Name() {
		t.Fatalf("next request reselected vetoed UDP exit %q", got)
	}
}

func TestSmartUDPNoResponseRequiresUnansweredMeaningfulTraffic(t *testing.T) {
	for _, tc := range []struct {
		name     string
		upload   int64
		download int64
	}{
		{name: "idle"},
		{name: "tiny upload", upload: smartMonitorMinUploadBytes - 1},
		{name: "responded then idle", upload: 6714, download: 1200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadata := &C.Metadata{NetWork: C.UDP}
			conn := &smartMonitoredTraffic{metadata: metadata}
			start := time.Now()
			conn.observeTotals(tc.upload, tc.download, start)
			conn.observeTotals(tc.upload, tc.download, start.Add(2*smartMonitorNoResponseAfter))
			if signal, ok := conn.sample(start.Add(2 * smartMonitorNoResponseAfter)); ok {
				t.Fatalf("healthy or idle UDP produced veto evidence: %s", signal)
			}
		})
	}
}
