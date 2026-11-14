package outboundgroup

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/callback"
	"github.com/metacubex/mihomo/component/smart"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

const (
	smartMonitorInterval           = time.Second
	smartMonitorEvidenceWindow     = 2 * time.Minute
	smartMonitorCooldown           = 5 * time.Minute
	smartMonitorNoResponseAfter    = 5 * time.Second
	smartMonitorSlowResponseAfter  = 2500 * time.Millisecond
	smartMonitorRequestSeparation  = 500 * time.Millisecond
	smartMonitorMinUploadBytes     = 128
	smartMonitorMinDownloadBytes   = 16 * 1024
	smartMonitorSlowBytesPerSecond = 16 * 1024
)

type smartActiveMonitor struct {
	mu          sync.Mutex
	connections map[*smartMonitoredTraffic]struct{}
	evidence    map[smartDegradationKey]smartDegradationEvidence
}

type smartDegradationEvidence struct {
	connection     *smartMonitoredTraffic
	observedAt     time.Time
	confirmedUntil time.Time
}

type smartMonitorSignal struct {
	reason        string
	observation   uint64
	duration      time.Duration
	uploadBytes   int64
	downloadBytes int64
}

type smartMonitoredConn struct {
	C.Conn
	*smartMonitoredTraffic
}

type smartMonitoredTraffic struct {
	smart          *Smart
	proxyName      string
	target         string
	wildcardTarget string
	metadata       *C.Metadata
	tracker        statistic.Tracker
	videoCDN       bool

	mu               sync.Mutex
	closed           bool
	observing        bool
	signaled         bool
	observation      uint64
	startedAt        time.Time
	lastReadAt       time.Time
	uploadBaseline   int64
	downloadBaseline int64
	uploadTotal      int64
	downloadTotal    int64
	lastSample       time.Time
	slowSince        time.Time
	slowUpload       int64
	slowDownload     int64
}

func (s *Smart) startActiveConnectionMonitor() {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		waitTicker := time.NewTicker(100 * time.Millisecond)
		defer waitTicker.Stop()
		for tunnel.Status() != tunnel.Running {
			select {
			case <-waitTicker.C:
			case <-s.ctx.Done():
				return
			}
		}

		ticker := time.NewTicker(smartMonitorInterval)
		defer ticker.Stop()
		for {
			select {
			case now := <-ticker.C:
				s.sampleActiveConnections(now)
			case <-s.ctx.Done():
				return
			}
		}
	}()
}

func (s *Smart) monitorActiveConnection(conn C.Conn, proxy C.Proxy, metadata *C.Metadata) C.Conn {
	if metadata == nil || metadata.NetWork != C.TCP || metadata.DstPort != 443 || metadata.Type == C.INNER || metadata.WildcardTarget == "" {
		return conn
	}

	return &smartMonitoredConn{Conn: conn, smartMonitoredTraffic: s.trackActiveTraffic(proxy, metadata)}
}

func (s *Smart) monitorActivePacketConnection(conn C.PacketConn, proxy C.Proxy, metadata *C.Metadata) C.PacketConn {
	if metadata == nil || metadata.NetWork != C.UDP || metadata.DstPort != 443 || metadata.Type == C.INNER || metadata.WildcardTarget != smartVideoWildcard {
		return conn
	}
	traffic := s.trackActiveTraffic(proxy, metadata)
	return callback.NewCloseCallbackPacketConn(conn, traffic.close)
}

const smartVideoWildcard = "*.googlevideo.com"

func smartVideoCDN(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	return host == "googlevideo.com" || strings.HasSuffix(host, ".googlevideo.com")
}

func smartWinnerCacheScope(metadata *C.Metadata, asnNumber string) (string, string) {
	if metadata.WildcardTarget == smartVideoWildcard {
		return smart.FormatDBKey(metadata.SmartTarget, smartVideoWildcard, metadata.NetWork.String()), ""
	}
	return metadata.SmartTarget, asnNumber
}

func (s *Smart) storeDialWinner(metadata *C.Metadata, asnNumber string, proxy C.Proxy) {
	target, cacheASN := smartWinnerCacheScope(metadata, asnNumber)
	if metadata.WildcardTarget == smartVideoWildcard {
		s.activeMonitor.mu.Lock()
		defer s.activeMonitor.mu.Unlock()
		key := smartDegradationKey{node: proxy.Name(), wildcard: smartVideoWildcard, network: metadata.NetWork}
		if time.Now().Before(s.activeMonitor.evidence[key].confirmedUntil) {
			return
		}
	}
	s.store.StoreUnwrapResult(s.Name(), s.configName, target, cacheASN, metadata.WildcardTarget, []C.Proxy{proxy})
}

func (s *Smart) trackActiveTraffic(proxy C.Proxy, metadata *C.Metadata) *smartMonitoredTraffic {
	monitored := &smartMonitoredTraffic{
		smart:          s,
		proxyName:      proxy.Name(),
		target:         metadata.SmartTarget,
		wildcardTarget: metadata.WildcardTarget,
		metadata:       metadata,
		videoCDN:       metadata.WildcardTarget == smartVideoWildcard,
	}
	s.activeMonitor.mu.Lock()
	if s.activeMonitor.connections == nil {
		s.activeMonitor.connections = make(map[*smartMonitoredTraffic]struct{})
	}
	s.activeMonitor.connections[monitored] = struct{}{}
	s.activeMonitor.mu.Unlock()
	return monitored
}

func (c *smartMonitoredConn) Close() error {
	c.smartMonitoredTraffic.close()
	return c.Conn.Close()
}

func (c *smartMonitoredTraffic) close() {
	c.mu.Lock()
	remove := !c.closed
	if remove {
		c.closed = true
	}
	c.mu.Unlock()
	if remove {
		c.smart.removeActiveConnection(c)
	}
}

func (c *smartMonitoredConn) ReaderReplaceable() bool {
	return true
}

func (c *smartMonitoredConn) WriterReplaceable() bool {
	return true
}

func (c *smartMonitoredConn) Upstream() any {
	return c.Conn
}

func (c *smartMonitoredTraffic) observeTotals(uploadTotal, downloadTotal int64, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	previousDownloadTotal := c.downloadTotal
	uploadDelta := uploadTotal - c.uploadTotal
	downloadDelta := downloadTotal - c.downloadTotal
	c.uploadTotal = uploadTotal
	c.downloadTotal = downloadTotal
	if c.videoCDN {
		c.observeVideoTraffic(uploadDelta, downloadDelta, now)
		return
	}
	if downloadDelta > 0 {
		c.lastReadAt = now
	}
	if uploadDelta <= 0 {
		return
	}
	if !c.observing || c.signaled || (!c.lastReadAt.IsZero() && now.Sub(c.lastReadAt) >= smartMonitorRequestSeparation && downloadTotal > c.downloadBaseline) {
		c.observation++
		c.observing = true
		c.signaled = false
		c.startedAt = now
		c.uploadBaseline = uploadTotal - uploadDelta
		c.downloadBaseline = previousDownloadTotal
	}
}

// Only count windows where payload continues to arrive slowly. A pause at the
// player or an idle association cannot establish that the route is degraded.
func (c *smartMonitoredTraffic) observeVideoTraffic(uploadDelta, downloadDelta int64, now time.Time) {
	previous := c.lastSample
	c.lastSample = now
	if previous.IsZero() || downloadDelta <= 0 || float64(downloadDelta) >= smartMonitorSlowBytesPerSecond*now.Sub(previous).Seconds() {
		c.slowSince = time.Time{}
		c.slowUpload, c.slowDownload = 0, 0
		c.signaled = false
		return
	}
	if c.slowSince.IsZero() {
		c.slowSince = previous
	}
	c.slowUpload += uploadDelta
	c.slowDownload += downloadDelta
}

func (c *smartMonitoredTraffic) sample(now time.Time) (smartMonitorSignal, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.signaled {
		return smartMonitorSignal{}, false
	}
	if c.videoCDN {
		duration := now.Sub(c.slowSince)
		if c.slowSince.IsZero() || duration < smartMonitorSlowResponseAfter || c.slowDownload < smartMonitorMinDownloadBytes || c.slowUpload < smartMonitorMinUploadBytes {
			return smartMonitorSignal{}, false
		}
		c.observation++
		c.signaled = true
		return smartMonitorSignal{reason: "sustained-low-throughput", observation: c.observation, duration: duration,
			uploadBytes: c.slowUpload, downloadBytes: c.slowDownload}, true
	}
	if !c.observing {
		return smartMonitorSignal{}, false
	}

	duration := now.Sub(c.startedAt)
	upload := c.uploadTotal - c.uploadBaseline
	download := c.downloadTotal - c.downloadBaseline
	if upload < smartMonitorMinUploadBytes {
		return smartMonitorSignal{}, false
	}

	reason := ""
	if download == 0 && duration >= smartMonitorNoResponseAfter {
		reason = "no-response-after-write"
	} else if download >= smartMonitorMinDownloadBytes && duration >= smartMonitorSlowResponseAfter {
		rate := float64(download) / duration.Seconds()
		if rate < smartMonitorSlowBytesPerSecond {
			reason = "sustained-low-throughput"
		} else {
			c.observing = false
			return smartMonitorSignal{}, false
		}
	}
	if reason == "" {
		return smartMonitorSignal{}, false
	}

	c.signaled = true
	return smartMonitorSignal{
		reason:        reason,
		observation:   c.observation,
		duration:      duration,
		uploadBytes:   upload,
		downloadBytes: download,
	}, true
}

func (s *Smart) removeActiveConnection(conn *smartMonitoredTraffic) {
	s.activeMonitor.mu.Lock()
	delete(s.activeMonitor.connections, conn)
	s.activeMonitor.mu.Unlock()
}

func (s *Smart) sampleActiveConnections(now time.Time) {
	s.activeMonitor.mu.Lock()
	for key, evidence := range s.activeMonitor.evidence {
		if key.wildcard == smartVideoWildcard && !evidence.confirmedUntil.IsZero() {
			continue
		}
		if !now.Before(evidence.confirmedUntil) && now.Sub(evidence.observedAt) > smartMonitorEvidenceWindow {
			delete(s.activeMonitor.evidence, key)
		}
	}
	connections := make([]*smartMonitoredTraffic, 0, len(s.activeMonitor.connections))
	for conn := range s.activeMonitor.connections {
		connections = append(connections, conn)
	}
	s.activeMonitor.mu.Unlock()

	for _, conn := range connections {
		tracker := s.findActiveTracker(conn)
		if tracker == nil {
			continue
		}
		info := tracker.Info()
		conn.observeTotals(info.UploadTotal.Load(), info.DownloadTotal.Load(), now)
		if signal, ok := conn.sample(now); ok {
			s.handleActiveDegradation(conn, signal, now)
		}
	}
}

func (s *Smart) handleActiveDegradation(conn *smartMonitoredTraffic, signal smartMonitorSignal, now time.Time) {
	if conn.videoCDN && s.selected.Load() != "" {
		return
	}
	if signal.reason != "sustained-low-throughput" {
		log.Warnln("[SmartMonitor] Observed unanswered active connection without automatic action: group=[%s] node=[%s] wildcard=[%s] duration=[%s] upload=[%d] observation=[%d]",
			s.Name(), conn.proxyName, conn.wildcardTarget, signal.duration, signal.uploadBytes, signal.observation)
		return
	}
	key := smartDegradationKey{node: conn.proxyName, wildcard: conn.wildcardTarget, network: conn.metadata.NetWork}
	confirmed := s.recordActiveDegradationEvidence(key, conn, signal.reason, now)
	if !confirmed {
		log.Warnln("[SmartMonitor] Suspect active connection: group=[%s] node=[%s] wildcard=[%s] reason=[%s] duration=[%s] upload=[%d] download=[%d] observation=[%d]",
			s.Name(), conn.proxyName, conn.wildcardTarget, signal.reason, signal.duration, signal.uploadBytes, signal.downloadBytes, signal.observation)
		return
	}

	log.Warnln("[SmartMonitor] Confirmed active degradation: group=[%s] node=[%s] wildcard=[%s] reason=[%s] duration=[%s] upload=[%d] download=[%d] observation=[%d] network=[%s]",
		s.Name(), conn.proxyName, conn.wildcardTarget, signal.reason, signal.duration, signal.uploadBytes, signal.downloadBytes, signal.observation, key.network.String())

	if !conn.videoCDN {
		s.store.DeleteUnwrapResult(s.Name(), s.configName, conn.target, "", conn.wildcardTarget)
	}

	tracker := s.findActiveTracker(conn)
	if tracker == nil {
		log.Warnln("[SmartMonitor] Unable to close confirmed degraded connection: group=[%s] node=[%s] wildcard=[%s] error=[tracker-not-found]",
			s.Name(), conn.proxyName, conn.wildcardTarget)
		return
	}
	if err := statistic.CloseDegraded(tracker); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Warnln("[SmartMonitor] Failed to close confirmed degraded connection: id=[%s] error=[%v]", tracker.ID(), err)
	} else {
		log.Warnln("[SmartMonitor] Closed confirmed degraded connection: id=[%s] group=[%s] node=[%s] wildcard=[%s]",
			tracker.ID(), s.Name(), conn.proxyName, conn.wildcardTarget)
	}
}

func (s *Smart) recordActiveDegradationEvidence(key smartDegradationKey, conn *smartMonitoredTraffic, reason string, now time.Time) bool {
	if reason != "sustained-low-throughput" {
		return false
	}
	s.activeMonitor.mu.Lock()
	defer s.activeMonitor.mu.Unlock()
	if s.activeMonitor.evidence == nil {
		s.activeMonitor.evidence = make(map[smartDegradationKey]smartDegradationEvidence)
	}
	previous, found := s.activeMonitor.evidence[key]
	if found && now.Before(previous.confirmedUntil) {
		return false
	}
	withinWindow := found && now.Sub(previous.observedAt) <= smartMonitorEvidenceWindow
	if withinWindow && !conn.videoCDN {
		if previous.connection == conn || !previous.connection.isClosed() {
			return false
		}
	}
	if conn.videoCDN || withinWindow {
		s.activeMonitor.evidence[key] = smartDegradationEvidence{observedAt: now, confirmedUntil: now.Add(smartMonitorCooldown)}
		if conn.videoCDN {
			cacheTarget, _ := smartWinnerCacheScope(conn.metadata, "")
			s.store.DeleteUnwrapResult(s.Name(), s.configName, cacheTarget, "", key.wildcard)
		}
		return true
	}
	s.activeMonitor.evidence[key] = smartDegradationEvidence{
		connection: conn,
		observedAt: now,
	}
	return false
}

func (c *smartMonitoredTraffic) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

type smartDegradationKey struct {
	node     string
	wildcard string
	network  C.NetWork
}

func (s *Smart) activeNodeCoolingDown(proxyName, wildcardTarget string, network C.NetWork, now time.Time) bool {
	key := smartDegradationKey{node: proxyName, wildcard: wildcardTarget, network: network}
	s.activeMonitor.mu.Lock()
	defer s.activeMonitor.mu.Unlock()
	return now.Before(s.activeMonitor.evidence[key].confirmedUntil)
}

type smartVideoDegradation struct {
	Node       string    `json:"node"`
	Network    string    `json:"network"`
	ObservedAt time.Time `json:"observed_at"`
	Until      time.Time `json:"until"`
}

// Retain the latest confirmation while the node belongs to this group, so a
// scheduler slower than the cooldown can still see it on its next poll.
func (s *Smart) videoDegradations(now time.Time, proxies []C.Proxy) []smartVideoDegradation {
	entries := make([]smartVideoDegradation, 0)
	s.activeMonitor.mu.Lock()
	defer s.activeMonitor.mu.Unlock()
	if len(s.activeMonitor.evidence) == 0 {
		return entries
	}
	members := make(map[string]bool, len(proxies))
	for _, proxy := range proxies {
		members[proxy.Name()] = true
	}
	for key, evidence := range s.activeMonitor.evidence {
		if key.wildcard != smartVideoWildcard || evidence.confirmedUntil.IsZero() {
			continue
		}
		if !members[key.node] && !now.Before(evidence.confirmedUntil) {
			delete(s.activeMonitor.evidence, key)
			continue
		}
		entries = append(entries, smartVideoDegradation{Node: key.node, Network: key.network.String(), ObservedAt: evidence.observedAt, Until: evidence.confirmedUntil})
	}
	return entries
}

func (s *Smart) findActiveTracker(conn *smartMonitoredTraffic) statistic.Tracker {
	conn.mu.Lock()
	if conn.tracker != nil {
		tracker := conn.tracker
		conn.mu.Unlock()
		return tracker
	}
	conn.mu.Unlock()

	var matched statistic.Tracker
	statistic.DefaultManager.RangeSmartTarget(conn.target, func(id string) bool {
		tracker := statistic.DefaultManager.Get(id)
		if tracker != nil && tracker.Info().Metadata == conn.metadata {
			matched = tracker
			return false
		}
		return true
	})
	if matched != nil {
		conn.mu.Lock()
		conn.tracker = matched
		conn.mu.Unlock()
	}
	return matched
}

func (s smartMonitorSignal) String() string {
	return fmt.Sprintf("%s duration=%s upload=%d download=%d observation=%d", s.reason, s.duration, s.uploadBytes, s.downloadBytes, s.observation)
}
