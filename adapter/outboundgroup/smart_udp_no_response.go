package outboundgroup

import (
	"sort"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
)

// Unanswered UDP is negative evidence for future selections. It does not
// change association/election lifetimes, close trackers, or enter the shared
// TCP/UDP host-failure records. Eligibility overrides even a shared winner.
func (s *Smart) denyUnansweredUDP(conn *smartMonitoredTraffic, signal smartMonitorSignal, now time.Time) {
	if s.selected.Load() != "" {
		return
	}
	key := smartDegradationKey{node: conn.proxyName, wildcard: conn.wildcardTarget, network: C.UDP}
	s.activeMonitor.mu.Lock()
	if now.Before(s.activeMonitor.unansweredUDP[key].confirmedUntil) {
		s.activeMonitor.mu.Unlock()
		return
	}
	if s.activeMonitor.unansweredUDP == nil {
		s.activeMonitor.unansweredUDP = make(map[smartDegradationKey]smartDegradationEvidence)
	}
	for oldKey, evidence := range s.activeMonitor.unansweredUDP {
		if !now.Before(evidence.confirmedUntil) {
			delete(s.activeMonitor.unansweredUDP, oldKey)
		}
	}
	until := now.Add(smartMonitorCooldown)
	s.activeMonitor.unansweredUDP[key] = smartDegradationEvidence{observedAt: now, confirmedUntil: until}
	s.activeMonitor.mu.Unlock()
	log.Warnln("[SmartMonitor] Denied unanswered UDP exit: group=[%s] node=[%s] wildcard=[%s] target=[%s] duration=[%s] upload=[%d] until=[%s]",
		s.Name(), conn.proxyName, conn.wildcardTarget, conn.target, signal.duration, signal.uploadBytes, until.Format(time.RFC3339))
}

func (s *Smart) unansweredUDPCoolingDown(proxyName, wildcardTarget string, now time.Time) bool {
	key := smartDegradationKey{node: proxyName, wildcard: wildcardTarget, network: C.UDP}
	s.activeMonitor.mu.Lock()
	defer s.activeMonitor.mu.Unlock()
	return now.Before(s.activeMonitor.unansweredUDP[key].confirmedUntil)
}

type smartUDPNoResponse struct {
	Node       string    `json:"node"`
	Wildcard   string    `json:"wildcard"`
	ObservedAt time.Time `json:"observed_at"`
	Until      time.Time `json:"until"`
}

func (s *Smart) udpNoResponses(now time.Time, proxies []C.Proxy) []smartUDPNoResponse {
	entries := make([]smartUDPNoResponse, 0)
	s.activeMonitor.mu.Lock()
	defer s.activeMonitor.mu.Unlock()
	members := make(map[string]bool, len(proxies))
	for _, proxy := range proxies {
		members[proxy.Name()] = true
	}
	for key, evidence := range s.activeMonitor.unansweredUDP {
		if !now.Before(evidence.confirmedUntil) {
			delete(s.activeMonitor.unansweredUDP, key)
			continue
		}
		if !members[key.node] {
			continue
		}
		entries = append(entries, smartUDPNoResponse{Node: key.node, Wildcard: key.wildcard, ObservedAt: evidence.observedAt, Until: evidence.confirmedUntil})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Node != entries[j].Node {
			return entries[i].Node < entries[j].Node
		}
		return entries[i].Wildcard < entries[j].Wildcard
	})
	return entries
}
