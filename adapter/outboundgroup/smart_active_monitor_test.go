package outboundgroup

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/component/smart"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

type smartMonitorTestConn struct {
	N.ExtendedConn
}

func (*smartMonitorTestConn) Chains() C.Chain               { return nil }
func (*smartMonitorTestConn) ProviderChains() C.Chain       { return nil }
func (*smartMonitorTestConn) AppendToChains(C.ProxyAdapter) {}
func (*smartMonitorTestConn) RemoteDestination() string     { return "" }

func TestSmartMonitoredConnPreservesRelayUnwrap(t *testing.T) {
	upstreamReader, upstreamWriter := net.Pipe()
	defer upstreamReader.Close()
	defer upstreamWriter.Close()
	base := N.NewExtendedConn(upstreamReader)
	inner := &smartMonitorTestConn{ExtendedConn: base}
	monitored := &smartMonitoredConn{Conn: inner}

	if got := N.UnwrapReader(monitored); got != inner {
		t.Fatalf("reader did not unwrap to base connection: %T", got)
	}
	if got := N.UnwrapWriter(monitored); got != inner {
		t.Fatalf("writer did not unwrap to base connection: %T", got)
	}
}

func TestSmartMonitoredConnNoResponseNeedsMeaningfulWrite(t *testing.T) {
	start := time.Unix(100, 0)
	conn := &smartMonitoredTraffic{}

	conn.observeTotals(smartMonitorMinUploadBytes-1, 0, start)
	if signal, ok := conn.sample(start.Add(smartMonitorNoResponseAfter)); ok {
		t.Fatalf("tiny write produced signal: %s", signal)
	}

	conn.observeTotals(smartMonitorMinUploadBytes, 0, start.Add(time.Second))
	signal, ok := conn.sample(start.Add(time.Second + smartMonitorNoResponseAfter))
	if !ok || signal.reason != "no-response-after-write" {
		t.Fatalf("meaningful unanswered write did not produce expected signal: ok=%v signal=%s", ok, signal)
	}
}

func TestSmartMonitoredConnDistinguishesSlowAndHealthyTransfer(t *testing.T) {
	start := time.Unix(200, 0)
	slow := &smartMonitoredTraffic{}
	slow.observeTotals(smartMonitorMinUploadBytes, 0, start)
	slow.observeTotals(smartMonitorMinUploadBytes, smartMonitorMinDownloadBytes, start.Add(time.Second))
	signal, ok := slow.sample(start.Add(3 * time.Second))
	if !ok || signal.reason != "sustained-low-throughput" {
		t.Fatalf("slow transfer did not produce expected signal: ok=%v signal=%s", ok, signal)
	}

	healthy := &smartMonitoredTraffic{}
	healthy.observeTotals(smartMonitorMinUploadBytes, 0, start)
	healthy.observeTotals(smartMonitorMinUploadBytes, 128*1024, start.Add(time.Second))
	if signal, ok := healthy.sample(start.Add(3 * time.Second)); ok {
		t.Fatalf("healthy transfer produced signal: %s", signal)
	}
}

func TestSmartOrdinaryTLSLowThroughputKeepsTrackerAndWinner(t *testing.T) {
	smart.InitCache()
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
	proxy := smartVideoTestProxy{name: "working"}
	metadata := C.Metadata{NetWork: C.TCP, Type: C.TUN, DstPort: 443, WildcardTarget: "*.example.com", SmartTarget: "stream"}
	s.storeDialWinner(&metadata, "", proxy, nil)

	observe := func() statistic.Tracker {
		target := metadata
		local, remote := net.Pipe()
		t.Cleanup(func() { _ = remote.Close() })
		conn := s.monitorActiveConnection(&smartMonitorTestConn{ExtendedConn: N.NewExtendedConn(local)}, proxy, &target)
		tracker := statistic.NewTCPTracker(conn, statistic.DefaultManager, &target, nil, 0, 0, true)
		t.Cleanup(func() { _ = tracker.Close() })
		monitored := conn.(*smartMonitoredConn).smartMonitoredTraffic
		s.handleActiveDegradation(monitored, smartMonitorSignal{reason: "sustained-low-throughput"}, time.Now())
		return tracker
	}

	initial := observe()
	_ = initial.Close()
	active := observe()
	if statistic.DefaultManager.Get(active.ID()) == nil || active.Info().ClosedForDegradation() {
		t.Fatal("low-volume interactive connection was closed")
	}
	if result, _ := s.store.GetUnwrapResult(s.Name(), s.configName, metadata.SmartTarget, ""); result == nil || result.Proxy != proxy.Name() {
		t.Fatalf("observation changed the winner: %v", result)
	}
	if s.activeNodeCoolingDown(proxy.Name(), metadata.WildcardTarget, metadata.NetWork, time.Now()) {
		t.Fatal("low-volume interactive connection put its exit in cooldown")
	}
}

func TestSmartSustainedVideoTCPStillClosesDegradedTracker(t *testing.T) {
	smart.InitCache()
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
	proxy := smartVideoTestProxy{name: "slow-video"}
	metadata := &C.Metadata{NetWork: C.TCP, Type: C.TUN, DstPort: 443, SmartTarget: "video", WildcardTarget: smartVideoWildcard}
	s.storeDialWinner(metadata, "", proxy, nil)
	local, remote := net.Pipe()
	t.Cleanup(func() { _ = remote.Close() })
	conn := s.monitorActiveConnection(&smartMonitorTestConn{ExtendedConn: N.NewExtendedConn(local)}, proxy, metadata)
	tracker := statistic.NewTCPTracker(conn, statistic.DefaultManager, metadata, nil, 0, 0, true)
	t.Cleanup(func() { _ = tracker.Close() })
	start := time.Now()
	s.sampleActiveConnections(start)
	for second := 1; second <= 3; second++ {
		tracker.UploadTotal.Add(256)
		tracker.DownloadTotal.Add(8 * 1024)
		s.sampleActiveConnections(start.Add(time.Duration(second) * time.Second))
	}
	if statistic.DefaultManager.Get(tracker.ID()) != nil || !tracker.Info().ClosedForDegradation() {
		t.Fatal("continuous low-rate video did not close its tracker")
	}
	target, _ := smartWinnerCacheScope(metadata, "")
	if result, _ := s.store.GetUnwrapResult(s.Name(), s.configName, target, ""); result != nil {
		t.Fatalf("confirmed video degradation kept its winner: %v", result)
	}
	if !s.activeNodeCoolingDown(proxy.Name(), smartVideoWildcard, C.TCP, start.Add(3*time.Second)) {
		t.Fatal("confirmed video degradation did not enter TCP cooldown")
	}
}

func TestSmartActiveDegradationDoesNotConfirmNoResponseEvidence(t *testing.T) {
	s := &Smart{}
	first := &smartMonitoredTraffic{}
	second := &smartMonitoredTraffic{}
	now := time.Unix(450, 0)
	key := smartDegradationKey{node: "node", wildcard: "*.example.com", network: C.TCP}

	if s.recordActiveDegradationEvidence(key, first, "no-response-after-write", now) {
		t.Fatal("first no-response observation confirmed degradation")
	}
	if s.recordActiveDegradationEvidence(key, second, "no-response-after-write", now.Add(time.Second)) {
		t.Fatal("concurrent no-response connection confirmed degradation")
	}
	first.closed = true
	if s.recordActiveDegradationEvidence(key, second, "no-response-after-write", now.Add(2*time.Second)) {
		t.Fatal("no-response evidence confirmed degradation")
	}
}

func TestSmartActiveDegradationCooldownIsScopedByNodeAndWildcard(t *testing.T) {
	s := &Smart{}
	now := time.Unix(500, 0)
	key := smartDegradationKey{node: "node-a", wildcard: "*.example.com", network: C.TCP}
	s.activeMonitor.evidence = map[smartDegradationKey]smartDegradationEvidence{
		key: {confirmedUntil: now.Add(smartMonitorCooldown)},
	}

	if !s.activeNodeCoolingDown("node-a", "*.example.com", C.TCP, now) {
		t.Fatal("confirmed node and wildcard were not cooling down")
	}
	if s.activeNodeCoolingDown("node-b", "*.example.com", C.TCP, now) {
		t.Fatal("cooldown leaked to another node")
	}
	if s.activeNodeCoolingDown("node-a", "*.other.example", C.TCP, now) {
		t.Fatal("cooldown leaked to another wildcard")
	}
	if s.activeNodeCoolingDown("node-a", "*.example.com", C.TCP, now.Add(smartMonitorCooldown)) {
		t.Fatal("expired cooldown remained active")
	}
}

type smartMonitorTestPacketConn struct {
	C.PacketConn
	closed bool
}

func (pc *smartMonitorTestPacketConn) Close() error               { pc.closed = true; return nil }
func (*smartMonitorTestPacketConn) Chains() C.Chain               { return nil }
func (*smartMonitorTestPacketConn) ProviderChains() C.Chain       { return nil }
func (*smartMonitorTestPacketConn) AppendToChains(C.ProxyAdapter) {}
func (*smartMonitorTestPacketConn) RemoteDestination() string     { return "" }

type smartVideoTestProxy struct {
	C.Proxy
	name   string
	udp    bool
	packet *smartMonitorTestPacketConn
}

func (p smartVideoTestProxy) Name() string                      { return p.name }
func (p smartVideoTestProxy) SupportUDP() bool                  { return p.udp }
func (p smartVideoTestProxy) AliveForTestUrl(string) bool       { return true }
func (p smartVideoTestProxy) LastDelayForTestUrl(string) uint16 { return 10 }
func (p smartVideoTestProxy) Type() C.AdapterType               { return C.Socks5 }
func (p smartVideoTestProxy) ListenPacketContext(context.Context, *C.Metadata) (C.PacketConn, error) {
	return p.packet, nil
}

func TestSmartSustainedVideoUDPReselectsAndPublishesEvidence(t *testing.T) {
	smart.InitCache()
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart,
		EmptyFallback: smartVideoTestProxy{name: "REJECT"}}), store: &smart.Store{}}
	stalled := smartVideoTestProxy{name: "slow", udp: true}
	working := smartVideoTestProxy{name: "working", udp: true, packet: &smartMonitorTestPacketConn{}}
	httpOnly := smartVideoTestProxy{name: "tcp-only"}
	s.GroupBase.providerProxies = []C.Proxy{stalled, working, httpOnly}
	metadata := &C.Metadata{NetWork: C.UDP, Type: C.TUN, DstPort: 443, SniffHost: "rr5---sn-test.googlevideo.com", SmartTarget: "GeoSite [youtube]"}
	s.selectProxies(metadata, s.GetProxies(false))
	if metadata.WildcardTarget != smartVideoWildcard {
		t.Fatalf("sniffed CDN host selected %q", metadata.WildcardTarget)
	}
	s.storeDialWinner(metadata, "12345", stalled, nil)
	tcp := *metadata
	tcp.NetWork = C.TCP
	s.storeDialWinner(&tcp, "12345", working, nil)
	pc := &smartMonitorTestPacketConn{}
	tracker := statistic.NewUDPTracker(s.monitorActivePacketConnection(pc, stalled, metadata), statistic.DefaultManager, metadata, nil, 0, 0, true)
	t.Cleanup(func() { _ = tracker.Close() })
	start := time.Now().Add(-6 * time.Second)
	s.sampleActiveConnections(start)
	for second := 1; second <= 2; second++ {
		tracker.UploadTotal.Add(256)
		tracker.DownloadTotal.Add(8 * 1024)
		s.sampleActiveConnections(start.Add(time.Duration(second) * time.Second))
	}
	if pc.closed {
		t.Fatal("brief slow video samples closed the flow")
	}
	tracker.UploadTotal.Add(256)
	tracker.DownloadTotal.Add(8 * 1024)
	s.sampleActiveConnections(start.Add(3 * time.Second))
	if !pc.closed || statistic.DefaultManager.Get(tracker.ID()) != nil || !tracker.Info().ClosedForDegradation() {
		t.Fatal("sustained low throughput did not close the tracked video flow")
	}
	cacheTarget, _ := smartWinnerCacheScope(metadata, "12345")
	s.storeDialWinner(metadata, "12345", stalled, nil)
	if got, _ := s.store.GetUnwrapResult(s.Name(), s.configName, cacheTarget, ""); got != nil {
		t.Fatalf("cooling UDP winner repopulated the cache: %v", got)
	}
	nextMetadata := *metadata
	nextMetadata.UUID = ""
	next, err := s.ListenPacketContext(context.Background(), &nextMetadata)
	if err != nil {
		t.Fatalf("new video UDP association: %v", err)
	}
	t.Cleanup(func() { _ = next.Close() })
	if got, _ := s.store.GetUnwrapResult(s.Name(), s.configName, cacheTarget, ""); got == nil || got.Proxy != working.name {
		t.Fatalf("video UDP did not reselect a capable exit: %v", got)
	}
	selection, err := s.prepareDialSelection(context.Background(), &tcp)
	if err != nil || len(selection.proxies) != 1 || selection.proxies[0].Name() != working.name || selection.flight != nil {
		t.Fatalf("UDP cooldown changed the TCP winner: %v, %v", selection.proxies, err)
	}
	data, err := s.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var group struct {
		Degradations []smartVideoDegradation `json:"videoDegradations"`
	}
	if err := json.Unmarshal(data, &group); err != nil || len(group.Degradations) != 1 || group.Degradations[0].Node != stalled.name || group.Degradations[0].Network != "udp" {
		t.Fatalf("invalid published degradation: %s, %v", data, err)
	}
	afterCooldown := start.Add(3*time.Second + smartMonitorCooldown)
	if got := s.videoDegradations(afterCooldown, s.GetProxies(false)); len(got) != 1 || !got[0].Until.Equal(afterCooldown) {
		t.Fatalf("scheduler lost the most recent degradation after cooldown: %v", got)
	}
	s.GroupBase.providerProxies = []C.Proxy{stalled, httpOnly}
	if got := s.Unwrap(&nextMetadata, false); got != nil {
		t.Fatalf("no eligible UDP exit, but unwrap returned %v", got)
	}
	if _, err := s.ListenPacketContext(context.Background(), &nextMetadata); err == nil {
		t.Fatal("accepted UDP without an eligible exit")
	}
	if got := s.videoDegradations(afterCooldown, []C.Proxy{working}); len(got) != 0 {
		t.Fatalf("expired video evidence for a removed node remained: %v", got)
	}
}

func TestSmartVideoIdleAndBurstyTrafficDoesNotConfirmDegradation(t *testing.T) {
	start := time.Unix(900, 0)
	conn := &smartMonitoredTraffic{videoCDN: true}
	for i, downloaded := range []int64{0, 8 << 10, 16 << 10, 16 << 10, 16 << 10, 2 << 20, 2 << 20, 2<<20 + 8<<10} {
		now := start.Add(time.Duration(i) * time.Second)
		conn.observeTotals(int64(i)*256, downloaded, now)
		if signal, ok := conn.sample(now); ok {
			t.Fatalf("sample %d from idle or bursty video produced %v", i, signal)
		}
	}
}
