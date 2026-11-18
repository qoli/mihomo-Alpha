package outboundgroup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/smart"
	C "github.com/metacubex/mihomo/constant"
)

type smartPriorityTestProxy struct {
	smartVideoTestProxy
	alive   bool
	history []C.DelayHistory
}

func (p smartPriorityTestProxy) AliveForTestUrl(string) bool { return p.alive }
func (p smartPriorityTestProxy) DelayHistoryForTestUrl(string) []C.DelayHistory {
	return p.history
}

func TestSmartUDPFallbacksRequireUDPSupport(t *testing.T) {
	smart.InitCache()
	for _, tc := range []struct {
		name  string
		alive bool
	}{
		{name: "healthy with target failures", alive: true},
		{name: "unhealthy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
			metadata := &C.Metadata{NetWork: C.UDP, Host: "grok.com", DstPort: 443}
			metadata.WildcardTarget = smart.GetEffectiveTarget(metadata.Host, metadata.DstIP.String())
			tcpOnly := smartPriorityTestProxy{smartVideoTestProxy: smartVideoTestProxy{name: "tcp-only", packet: &smartMonitorTestPacketConn{}}, alive: tc.alive}
			udp := smartPriorityTestProxy{smartVideoTestProxy: smartVideoTestProxy{name: "udp", udp: true}, alive: tc.alive}
			all := []C.Proxy{tcpOnly, udp}
			if tc.alive {
				// Both nodes pass the URL check, but the target has a recorded failure on each.
				for _, p := range all {
					s.store.UpdateHostStatus(s.Name(), s.configName, metadata.WildcardTarget, metadata, p.Name(), 1, s.hostFailLimit, true, true, 1)
				}
			}

			got := s.filterProxies(metadata, nil, nil, all, maxSelected, smartSelectionOptions{})
			if len(got) != 1 || got[0].Name() != udp.Name() {
				t.Fatalf("UDP fallback candidates = %v, want only %s", got, udp.Name())
			}
			tcpMetadata := *metadata
			tcpMetadata.NetWork = C.TCP
			got = s.filterProxies(&tcpMetadata, nil, nil, all, maxSelected, smartSelectionOptions{})
			if len(got) != 2 || got[0].Name() != tcpOnly.Name() {
				t.Fatalf("TCP fallback candidates = %v, want both proxies with %s first", got, tcpOnly.Name())
			}

			s.providerProxies = all[:1]
			pc, err := s.ListenPacketContext(context.Background(), metadata)
			if pc != nil || err == nil || !strings.Contains(err.Error(), "no fresh proxy") {
				t.Fatalf("ListenPacketContext with only a TCP proxy = %v, %v, want no fresh proxy", pc, err)
			}
		})
	}
}

type smartSelectionWaitContext struct {
	context.Context
	onWait func()
}

func (ctx smartSelectionWaitContext) Done() <-chan struct{} {
	ctx.onWait()
	return ctx.Context.Done()
}

func TestSmartConcurrentSelectionSeparatesTCPAndUDP(t *testing.T) {
	smart.InitCache()
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
	tcpOnly := smartVideoTestProxy{name: "tcp-only"}
	udp := smartVideoTestProxy{name: "udp", udp: true}
	s.providerProxies = []C.Proxy{tcpOnly, udp}

	ctx := context.Background()
	target := "GeoSite [geolocation-!cn]"
	t.Cleanup(func() { s.store.DeleteUnwrapResult(s.Name(), s.configName, target, "", nil) })
	tcpMetadata := C.Metadata{NetWork: C.TCP, Host: "apple-relay.cloudflare.com", DstPort: 443, SmartTarget: target}
	udpMetadata := tcpMetadata
	udpMetadata.NetWork = C.UDP
	tcpLeader, err := s.prepareDialSelection(ctx, &tcpMetadata)
	if err != nil || tcpLeader.flight == nil || len(tcpLeader.proxies) == 0 || tcpLeader.proxies[0].Name() != tcpOnly.Name() {
		t.Fatalf("TCP leader selection = %+v, %v, want %s first", tcpLeader, err, tcpOnly.Name())
	}

	publish := func(metadata *C.Metadata, selection smartDialSelection, proxy C.Proxy) {
		s.updateDialWinner(metadata, selection, proxy)
		s.finishDialSelection(ctx, selection, proxy.Name(), nil)
	}
	// Keep the TCP election pending while UDP selects. If UDP joins the TCP
	// election, release it with the TCP-only winner so the regression fails.
	waitedForTCP := false
	waitCtx := smartSelectionWaitContext{Context: ctx, onWait: func() {
		waitedForTCP = true
		publish(&tcpMetadata, tcpLeader, tcpOnly)
	}}
	udpLeader, err := s.prepareDialSelection(waitCtx, &udpMetadata)
	if waitedForTCP || err != nil || udpLeader.flight == nil || len(udpLeader.proxies) != 1 || udpLeader.proxies[0].Name() != udp.Name() {
		t.Fatalf("UDP selection = %+v, %v (waited for TCP: %t), want independent election with only %s", udpLeader, err, waitedForTCP, udp.Name())
	}

	for _, tc := range []struct {
		metadata  *C.Metadata
		selection smartDialSelection
		winner    C.Proxy
	}{
		{&tcpMetadata, tcpLeader, tcpOnly},
		{&udpMetadata, udpLeader, udp},
	} {
		// A second request for each protocol still waits for its own leader.
		metadata := *tc.metadata
		waitCtx := smartSelectionWaitContext{Context: ctx, onWait: func() {
			publish(tc.metadata, tc.selection, tc.winner)
		}}
		follower, err := s.prepareDialSelection(waitCtx, &metadata)
		if err != nil || follower.flight != nil || len(follower.proxies) != 1 || follower.proxies[0].Name() != tc.winner.Name() {
			t.Fatalf("%s follower selection = %+v, %v, want published winner %s", metadata.NetWork, follower, err, tc.winner.Name())
		}
	}
}

type smartPacketDialTestProxy struct {
	smartVideoTestProxy
	listen func(context.Context) (C.PacketConn, error)
}

func (p smartPacketDialTestProxy) ListenPacketContext(ctx context.Context, _ *C.Metadata) (C.PacketConn, error) {
	return p.listen(ctx)
}

func TestSmartUDPSelectionLeavesTimeForNextCandidate(t *testing.T) {
	smart.InitCache()
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	firstStarted := make(chan struct{})
	stalled := smartPacketDialTestProxy{
		smartVideoTestProxy: smartVideoTestProxy{name: "stalled", udp: true},
		listen: func(attemptCtx context.Context) (C.PacketConn, error) {
			close(firstStarted)
			<-attemptCtx.Done()
			err := &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}
			t.Logf("first candidate: %v (request context: %v)", err, ctx.Err())
			return nil, err
		},
	}
	working := smartPacketDialTestProxy{
		smartVideoTestProxy: smartVideoTestProxy{name: "working", udp: true},
		listen: func(attemptCtx context.Context) (C.PacketConn, error) {
			t.Logf("second candidate context: %v", attemptCtx.Err())
			if err := attemptCtx.Err(); err != nil {
				return nil, err
			}
			return &smartMonitorTestPacketConn{}, nil
		},
	}
	s.providerProxies = []C.Proxy{stalled, working}
	target := "GeoSite [geolocation-!cn]"
	t.Cleanup(func() { s.store.DeleteUnwrapResult(s.Name(), s.configName, target, "", nil) })
	leaderMetadata := C.Metadata{NetWork: C.UDP, Host: "www.reddit.com", DstPort: 443, SmartTarget: target}
	followerMetadata := leaderMetadata
	followerMetadata.Host = "v.redd.it"
	leaderResult := make(chan error, 1)
	go func() {
		pc, err := s.ListenPacketContext(ctx, &leaderMetadata)
		if pc != nil {
			_ = pc.Close()
		}
		leaderResult <- err
	}()
	select {
	case <-firstStarted:
	case <-ctx.Done():
		t.Fatal("the first UDP candidate did not start")
	}

	followerCtx, cancelFollower := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelFollower()
	waited := false
	waitCtx := smartSelectionWaitContext{Context: followerCtx, onWait: func() { waited = true }}
	follower, followerErr := s.prepareDialSelection(waitCtx, &followerMetadata)
	if err := <-leaderResult; err != nil {
		t.Errorf("leader ListenPacketContext: %v", err)
	}
	if !waited || followerErr != nil || len(follower.proxies) != 1 || follower.proxies[0].Name() != working.Name() {
		t.Fatalf("follower selection = %+v, %v (waited: %t), want published winner %s", follower, followerErr, waited, working.Name())
	}
}

func TestSmartPriorityElectionReservesRoomForOtherTiers(t *testing.T) {
	smart.InitCache()
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart}), store: &smart.Store{}}
	applyPolicyPriority(s, "US:5;JP:4")
	metadata := &C.Metadata{NetWork: C.TCP, SmartTarget: "stream", WildcardTarget: "*.example.com"}
	var all []C.Proxy
	for i := 0; i < maxSelected+3; i++ {
		all = append(all, smartVideoTestProxy{name: fmt.Sprintf("US%d", i)})
	}
	// A group can include the same name through multiple providers.
	all = append(all, all[0], all[0])
	all = append(all, smartVideoTestProxy{name: "JP"}, smartVideoTestProxy{name: "NL"})
	selected := s.filterProxies(metadata, nil, nil, all, maxSelected, smartSelectionOptions{checkPriority: true, reserveWinner: "NL"})
	if len(selected) != maxSelected {
		t.Fatalf("selected %d candidates, want %d", len(selected), maxSelected)
	}
	seen := make(map[string]bool)
	for _, proxy := range selected {
		if seen[proxy.Name()] {
			t.Fatalf("selected two candidates named %s", proxy.Name())
		}
		seen[proxy.Name()] = true
	}
	if s.getPriorityFactor(selected[0].Name()) != 5 {
		t.Fatalf("expected a US candidate first, got %s", selected[0].Name())
	}
	for _, name := range []string{"JP", "NL"} {
		if !seen[name] {
			t.Fatalf("missing ranked or reserved candidate %s", name)
		}
	}
}

func TestSmartFixedSelectionBypassesPriorityElection(t *testing.T) {
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart})}
	applyPolicyPriority(s, "US:5")
	s.providerProxies = []C.Proxy{smartVideoTestProxy{name: "US"}, smartVideoTestProxy{name: "NL"}}
	s.ForceSet("NL")
	for _, network := range []C.NetWork{C.TCP, C.UDP} {
		selection, err := s.prepareDialSelection(context.Background(), &C.Metadata{NetWork: network, SmartTarget: "stream"})
		if err != nil || len(selection.proxies) != 1 || selection.proxies[0].Name() != "NL" || selection.flight != nil {
			t.Fatalf("fixed %s selection entered automatic priority election: %+v, %v", network, selection, err)
		}
	}
}

func TestSmartPriorityRecheckUsesElectionTimeAndRecovery(t *testing.T) {
	now := time.Now()
	s := &Smart{priorityRecheckInterval: time.Minute}
	applyPolicyPriority(s, "US:5;JP:4")
	entry := &smart.UnwrapResult{
		Proxy: "NL", SelectedAt: now, EligibleHigher: map[string]struct{}{"US": {}},
	}
	eligibility := smartProxyEligibility{smart: s, metadata: &C.Metadata{NetWork: C.TCP}, now: now}
	us := smartPriorityTestProxy{smartVideoTestProxy: smartVideoTestProxy{name: "US"}, alive: true}
	jp := smartPriorityTestProxy{smartVideoTestProxy: smartVideoTestProxy{name: "JP"}, alive: true}

	t.Run("healthy at election", func(t *testing.T) {
		if reason := s.priorityRecheck(now, entry, []C.Proxy{us}, eligibility); reason != "" {
			t.Fatalf("stable winner immediately re-elected: %s", reason)
		}
		if reason := s.priorityRecheck(now.Add(time.Minute), entry, []C.Proxy{us}, eligibility); reason != "priority-recheck-due" {
			t.Fatalf("expected periodic recheck, got %q", reason)
		}
	})
	t.Run("newly eligible", func(t *testing.T) {
		if reason := s.priorityRecheck(now, entry, []C.Proxy{us, jp}, eligibility); reason != "higher-priority-recovered" {
			t.Fatalf("expected early recheck, got %q", reason)
		}
	})
	t.Run("outage between requests", func(t *testing.T) {
		us.history = []C.DelayHistory{{Time: now.Add(time.Second)}, {Time: now.Add(2 * time.Second), Delay: 18}}
		if reason := s.priorityRecheck(now.Add(3*time.Second), entry, []C.Proxy{us}, eligibility); reason != "higher-priority-recovered" {
			t.Fatalf("expected recheck after outage, got %q", reason)
		}
	})
	t.Run("no healthy higher priority", func(t *testing.T) {
		us.alive = false
		eligibility.hostFailures = map[string]int{"JP": 1}
		if reason := s.priorityRecheck(now.Add(time.Minute), entry, []C.Proxy{us, jp}, eligibility); reason != "" {
			t.Fatalf("unhealthy candidates prompted recheck: %s", reason)
		}
	})
}

func TestShouldRecordSmartDialFailureIgnoresCanceledRaceLoser(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if shouldRecordSmartDialFailure(ctx, errors.New("transport returned after race cancellation")) {
		t.Fatal("canceled race loser must not be recorded as a node failure")
	}
	if !shouldRecordSmartDialFailure(context.Background(), errors.New("real dial failure")) {
		t.Fatal("live-context dial failure must be recorded")
	}
}

func TestSmartStatusProbeAcceptedTreatsEndpointPolicyAsReachable(t *testing.T) {
	for _, status := range []uint16{403, 405} {
		if !smartStatusProbeAccepted(status, false) {
			t.Fatalf("status %d must prove the proxy path is reachable", status)
		}
	}
	if smartStatusProbeAccepted(503, false) {
		t.Fatal("status 503 must remain an abnormal probe response")
	}
	if !smartStatusProbeAccepted(204, true) {
		t.Fatal("StatusTest success must remain accepted")
	}
}

func TestSmartSelectionCoordinatorPublishesOneWinner(t *testing.T) {
	var coordinator smartSelectionCoordinator
	const callers = 64

	start := make(chan struct{})
	results := make(chan string, callers)
	errs := make(chan error, callers)
	var leaders atomic.Int64
	var wg sync.WaitGroup

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			flight, leader := coordinator.begin("config/group/target")
			if leader {
				leaders.Add(1)
				time.Sleep(10 * time.Millisecond)
				coordinator.finish("config/group/target", flight, "winner", nil)
				results <- "winner"
				return
			}

			winner, err := flight.wait(context.Background())
			if err != nil {
				errs <- err
				return
			}
			results <- winner
		}()
	}

	close(start)
	wg.Wait()
	close(results)
	close(errs)

	if got := leaders.Load(); got != 1 {
		t.Fatalf("leaders = %d, want 1", got)
	}
	for err := range errs {
		t.Fatalf("wait error: %v", err)
	}
	count := 0
	for result := range results {
		count++
		if result != "winner" {
			t.Fatalf("winner = %q, want winner", result)
		}
	}
	if count != callers {
		t.Fatalf("results = %d, want %d", count, callers)
	}
}

func TestSmartSelectionCoordinatorPropagatesFailure(t *testing.T) {
	var coordinator smartSelectionCoordinator
	flight, leader := coordinator.begin("target")
	if !leader {
		t.Fatal("first caller must be leader")
	}

	wantErr := errors.New("selection failed")
	coordinator.finish("target", flight, "", wantErr)
	if _, err := flight.wait(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("wait error = %v, want %v", err, wantErr)
	}
}

func TestSmartSelectionCoordinatorRejectsMissingWinner(t *testing.T) {
	var coordinator smartSelectionCoordinator
	flight, leader := coordinator.begin("target")
	if !leader {
		t.Fatal("first caller must be leader")
	}

	coordinator.finish("target", flight, "", nil)
	if _, err := flight.wait(context.Background()); !errors.Is(err, errSmartSelectionMissingWinner) {
		t.Fatalf("wait error = %v, want %v", err, errSmartSelectionMissingWinner)
	}
}

func TestSmartSelectionCoordinatorWaitHonorsContext(t *testing.T) {
	var coordinator smartSelectionCoordinator
	flight, leader := coordinator.begin("target")
	if !leader {
		t.Fatal("first caller must be leader")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := flight.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("wait error = %v, want context canceled", err)
	}

	coordinator.finish("target", flight, "winner", nil)
}
