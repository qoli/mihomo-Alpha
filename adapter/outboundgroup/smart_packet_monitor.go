package outboundgroup

import (
	"errors"
	"net"
	"sync"
	"time"

	N "github.com/metacubex/mihomo/common/net"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel/statistic"
)

const smartPacketMinUnansweredUpload = 2 * 1200

var errSmartUDPNoResponse = errors.New("smart udp path produced no response")

type smartMonitoredPacketConn struct {
	C.PacketConn
	smart           *Smart
	proxyName       string
	target          string
	wildcardTarget  string
	asnNumber       string
	metadata        *C.Metadata
	onFirstResponse func(int64)
	onNoResponse    func(error)
	selectionOnce   sync.Once

	mu               sync.Mutex
	closed           bool
	signaled         bool
	responded        bool
	firstWriteAt     time.Time
	firstReadLatency int64
	uploadBytes      int64
	downloadBytes    int64
	outcomeErr       error
}

func (c *smartMonitoredPacketConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	n, err := c.PacketConn.WriteTo(b, addr)
	if n > 0 {
		c.mu.Lock()
		if c.firstWriteAt.IsZero() {
			c.firstWriteAt = time.Now()
		}
		c.uploadBytes += int64(n)
		c.mu.Unlock()
	}
	return n, err
}

func (c *smartMonitoredPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	if err == nil && n > 0 {
		c.observeResponse(n, time.Now())
	}
	return n, addr, err
}

func (c *smartMonitoredPacketConn) WaitReadFrom() ([]byte, func(), net.Addr, error) {
	data, put, addr, err := c.PacketConn.WaitReadFrom()
	if err == nil && len(data) > 0 {
		c.observeResponse(len(data), time.Now())
	}
	return data, put, addr, err
}

func (c *smartMonitoredPacketConn) observeResponse(size int, now time.Time) {
	c.mu.Lock()
	c.downloadBytes += int64(size)
	first := !c.responded && !c.signaled
	if first {
		c.responded = true
		if !c.firstWriteAt.IsZero() {
			c.firstReadLatency = now.Sub(c.firstWriteAt).Milliseconds()
		}
	}
	if c.signaled {
		c.responded = true
	}
	latency := c.firstReadLatency
	c.mu.Unlock()
	if first && c.onFirstResponse != nil {
		c.selectionOnce.Do(func() { c.onFirstResponse(latency) })
	}
}

func (c *smartMonitoredPacketConn) sample(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.signaled || c.responded || c.metadata == nil || c.metadata.DstPort != 443 || c.metadata.Type == C.INNER {
		return false
	}
	if c.firstWriteAt.IsZero() || c.uploadBytes < smartPacketMinUnansweredUpload || now.Sub(c.firstWriteAt) < smartMonitorNoResponseAfter {
		return false
	}
	c.signaled = true
	c.outcomeErr = errSmartUDPNoResponse
	return true
}

func (c *smartMonitoredPacketConn) Close() error {
	c.mu.Lock()
	remove := !c.closed
	closedBeforeResponse := !c.responded && !c.signaled
	c.closed = true
	c.mu.Unlock()
	if closedBeforeResponse && c.onNoResponse != nil {
		c.notifyNoResponse(net.ErrClosed)
	}
	if remove && c.smart != nil {
		c.smart.removeActivePacketConnection(c)
	}
	return c.PacketConn.Close()
}

func (c *smartMonitoredPacketConn) firstResponseLatency() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.firstReadLatency
}

func (c *smartMonitoredPacketConn) outcomeError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.outcomeErr
}

func (c *smartMonitoredPacketConn) notifyNoResponse(err error) {
	if c.onNoResponse != nil {
		c.selectionOnce.Do(func() { c.onNoResponse(err) })
	}
}

func (c *smartMonitoredPacketConn) ReaderReplaceable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.responded
}

func (c *smartMonitoredPacketConn) WriterReplaceable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.firstWriteAt.IsZero()
}

func (c *smartMonitoredPacketConn) Upstream() any { return c.PacketConn }

var _ C.PacketConn = (*smartMonitoredPacketConn)(nil)
var _ N.EnhancePacketConn = (*smartMonitoredPacketConn)(nil)

func (s *Smart) monitorActivePacketConnection(pc C.PacketConn, proxy C.Proxy, metadata *C.Metadata, asnNumber string, onFirstResponse func(int64), onNoResponse func(error)) *smartMonitoredPacketConn {
	monitored := &smartMonitoredPacketConn{
		PacketConn:      pc,
		smart:           s,
		proxyName:       proxy.Name(),
		target:          metadata.SmartTarget,
		wildcardTarget:  metadata.WildcardTarget,
		asnNumber:       asnNumber,
		metadata:        metadata,
		onFirstResponse: onFirstResponse,
		onNoResponse:    onNoResponse,
	}
	s.activeMonitor.mu.Lock()
	if s.activeMonitor.packets == nil {
		s.activeMonitor.packets = make(map[*smartMonitoredPacketConn]struct{})
	}
	s.activeMonitor.packets[monitored] = struct{}{}
	s.activeMonitor.mu.Unlock()
	return monitored
}

func (s *Smart) removeActivePacketConnection(conn *smartMonitoredPacketConn) {
	s.activeMonitor.mu.Lock()
	delete(s.activeMonitor.packets, conn)
	s.activeMonitor.mu.Unlock()
}

func (s *Smart) handleActivePacketNoResponse(conn *smartMonitoredPacketConn, now time.Time) {
	conn.mu.Lock()
	uploadBytes := conn.uploadBytes
	conn.mu.Unlock()
	log.Warnln("[SmartMonitor] Unanswered UDP/443 path: group=[%s] node=[%s] wildcard=[%s] upload=[%d] action=[cooldown-and-close]",
		s.Name(), conn.proxyName, conn.wildcardTarget, uploadBytes)
	s.coolDownActivePacketNode(conn.proxyName, conn.wildcardTarget, now)
	s.store.DeleteUnwrapResult(s.Name(), s.configName, conn.target, conn.asnNumber, conn.wildcardTarget)
	conn.notifyNoResponse(errSmartUDPNoResponse)

	tracker := s.findActivePacketTracker(conn)
	if tracker == nil {
		log.Warnln("[SmartMonitor] Unable to close unanswered UDP/443 path: group=[%s] node=[%s] wildcard=[%s] error=[tracker-not-found]",
			s.Name(), conn.proxyName, conn.wildcardTarget)
		return
	}
	if err := tracker.Close(); err != nil {
		log.Warnln("[SmartMonitor] Failed to close unanswered UDP/443 path: id=[%s] error=[%v]", tracker.ID(), err)
	}
}

func (s *Smart) findActivePacketTracker(conn *smartMonitoredPacketConn) statistic.Tracker {
	if conn.metadata == nil || conn.metadata.UUID == "" {
		return nil
	}
	return statistic.DefaultManager.Get(conn.metadata.UUID)
}

func (s *Smart) coolDownActivePacketNode(proxyName, wildcardTarget string, now time.Time) {
	key := proxyName + "\x00" + wildcardTarget
	s.activeMonitor.mu.Lock()
	if s.activeMonitor.packetCooldown == nil {
		s.activeMonitor.packetCooldown = make(map[string]time.Time)
	}
	s.activeMonitor.packetCooldown[key] = now.Add(smartMonitorCooldown)
	s.activeMonitor.mu.Unlock()
}

func (s *Smart) activePacketNodeCoolingDown(proxyName, wildcardTarget string, now time.Time) bool {
	key := proxyName + "\x00" + wildcardTarget
	s.activeMonitor.mu.Lock()
	defer s.activeMonitor.mu.Unlock()
	until, found := s.activeMonitor.packetCooldown[key]
	if !found {
		return false
	}
	if now.Before(until) {
		return true
	}
	delete(s.activeMonitor.packetCooldown, key)
	return false
}
