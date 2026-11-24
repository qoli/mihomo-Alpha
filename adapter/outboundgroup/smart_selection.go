package outboundgroup

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/metacubex/mihomo/component/smart"
	C "github.com/metacubex/mihomo/constant"
)

var errSmartSelectionMissingWinner = errors.New("smart selection completed without a winner")

type smartSelectionOptions struct {
	fresh         bool
	strictHealth  bool
	cachedOnly    bool
	checkPriority bool
	reserveWinner string
	dialID        uint64
}

type smartProxySelectionState struct {
	key          string
	cacheTarget  string
	cacheASN     string
	cached       *smart.UnwrapResult
	cacheExpired bool
	reselect     string
	fixed        bool
}

type smartDialSelection struct {
	proxies          []C.Proxy
	asnNumber        string
	cached           *smart.UnwrapResult
	fixed            bool
	eligiblePriority map[string]float64
	flightKey        string
	flight           *smartSelectionFlight
}

func shouldRecordSmartDialFailure(ctx context.Context, err error) bool {
	return smartDialOutcome(ctx, err) == "failed"
}

func smartStatusProbeAccepted(status uint16, ok bool) bool {
	if ok {
		return true
	}
	// StatusTest probes an arbitrary host root rather than the original request
	// path. A 403/405 response proves the proxy completed TCP, TLS, and HTTP; many
	// CDN roots intentionally return these statuses, so they cannot identify a
	// failed proxy node.
	return status == 403 || status == 405
}

type smartSelectionFlight struct {
	done   chan struct{}
	winner string
	err    error
}

type smartSelectionCoordinator struct {
	mu      sync.Mutex
	flights map[string]*smartSelectionFlight
}

func smartSelectionFlightKey(config, group, cacheTarget string, metadata *C.Metadata) string {
	// Coalesce across ASN changes, but keep different protocols and destination
	// address families from inheriting a leader's dial failure.
	addressType := metadata.AddrType()
	if addressType == C.AtypIPv6 && metadata.DstIP.Is4In6() {
		addressType = C.AtypIPv4
	}
	return smart.FormatDBKey(config, group, cacheTarget, metadata.NetWork.String(), addressType.String())
}

func (c *smartSelectionCoordinator) begin(key string) (*smartSelectionFlight, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.flights == nil {
		c.flights = make(map[string]*smartSelectionFlight)
	}
	if flight, found := c.flights[key]; found {
		return flight, false
	}

	flight := &smartSelectionFlight{done: make(chan struct{})}
	c.flights[key] = flight
	return flight, true
}

func (c *smartSelectionCoordinator) finish(key string, flight *smartSelectionFlight, winner string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	current, found := c.flights[key]
	if !found || current != flight {
		return
	}
	if err == nil && winner == "" {
		err = errSmartSelectionMissingWinner
	}

	flight.winner = winner
	flight.err = err
	delete(c.flights, key)
	close(flight.done)
}

func (f *smartSelectionFlight) wait(ctx context.Context) (string, error) {
	select {
	case <-f.done:
		if f.err != nil {
			return "", f.err
		}
		if f.winner == "" {
			return "", errSmartSelectionMissingWinner
		}
		return f.winner, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func findSmartProxyByName(proxies []C.Proxy, name string) (C.Proxy, error) {
	for _, proxy := range proxies {
		if proxy.Name() == name {
			return proxy, nil
		}
	}
	return nil, fmt.Errorf("smart selection winner unavailable: %s", name)
}

// priorityRecheck is evaluated only when a TCP request would reuse a winner.
// Eligibility changes can prompt a new election before the periodic recheck.
func (s *Smart) priorityRecheck(now time.Time, cached *smart.UnwrapResult, proxies []C.Proxy, eligibility smartProxyEligibility) string {
	factor := s.getPriorityFactor(cached.Proxy)
	due := !now.Before(cached.SelectedAt.Add(s.priorityRecheckInterval))
	for _, p := range proxies {
		if s.getPriorityFactor(p.Name()) <= factor || eligibility.exclusion(p) != "" {
			continue
		}
		if _, wasEligible := cached.EligibleHigher[p.Name()]; !wasEligible {
			return "higher-priority-recovered"
		}
		if due {
			return "priority-recheck-due"
		}
		// Existing URL-test history also exposes an outage and recovery that
		// occurred entirely between incoming connections.
		for _, sample := range p.DelayHistoryForTestUrl(s.testUrl) {
			if sample.Delay == 0 && sample.Time.After(cached.SelectedAt) {
				return "higher-priority-recovered"
			}
		}
	}
	return ""
}

// Keep a healthy representative of each available tier and, when eligible,
// the current winner. Use the remaining slots for the existing ranking.
func (s *Smart) limitPriorityCandidates(ranked []C.Proxy, limit int, winner string) []C.Proxy {
	if len(ranked) <= limit {
		return ranked
	}
	chosen := make(map[string]bool, limit)
	tiers := make(map[float64]bool)
	for _, p := range ranked {
		if p.Name() == winner {
			chosen[winner] = true
			tiers[s.getPriorityFactor(winner)] = true
			break
		}
	}
	for _, p := range ranked {
		if len(chosen) == limit {
			break
		}
		factor := s.getPriorityFactor(p.Name())
		if !tiers[factor] {
			chosen[p.Name()] = true
			tiers[factor] = true
		}
	}
	for _, p := range ranked {
		if len(chosen) == limit {
			break
		}
		chosen[p.Name()] = true
	}
	selected := make([]C.Proxy, 0, limit)
	for _, p := range ranked {
		if chosen[p.Name()] {
			selected = append(selected, p)
			delete(chosen, p.Name())
		}
	}
	return selected
}
