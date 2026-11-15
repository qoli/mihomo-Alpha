package outboundgroup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/metacubex/mihomo/component/smart"
	C "github.com/metacubex/mihomo/constant"
)

func TestSmartSelectionFlightKeyMatchesTargetCacheIdentity(t *testing.T) {
	const (
		config = "config"
		group  = "smart-group"
		target = "GeoSite [geolocation-!cn]"
	)

	got := smartSelectionFlightKey(config, group, target)
	want := "smart/config/smart-group/GeoSite [geolocation-!cn]"
	if got != want {
		t.Fatalf("smartSelectionFlightKey() = %q, want %q", got, want)
	}
}

type smartPriorityTestProxy struct {
	smartVideoTestProxy
	alive   bool
	history []C.DelayHistory
}

func (p smartPriorityTestProxy) AliveForTestUrl(string) bool { return p.alive }
func (p smartPriorityTestProxy) DelayHistoryForTestUrl(string) []C.DelayHistory {
	return p.history
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
	selection, err := s.prepareDialSelection(context.Background(), &C.Metadata{NetWork: C.TCP, SmartTarget: "stream"})
	if err != nil || len(selection.proxies) != 1 || selection.proxies[0].Name() != "NL" || selection.flight != nil {
		t.Fatalf("fixed selection entered automatic priority election: %+v, %v", selection, err)
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
