package outboundgroup

import (
	"errors"
	"net"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
)

func TestSmartPacketMonitorRequiresMeaningfulUDP443Upload(t *testing.T) {
	start := time.Unix(100, 0)
	conn := &smartMonitoredPacketConn{
		metadata:     &C.Metadata{NetWork: C.UDP, DstPort: 443},
		firstWriteAt: start,
		uploadBytes:  smartPacketMinUnansweredUpload - 1,
	}
	if conn.sample(start.Add(smartMonitorNoResponseAfter)) {
		t.Fatal("small unanswered upload was treated as a failed UDP path")
	}
	conn.uploadBytes = smartPacketMinUnansweredUpload
	if !conn.sample(start.Add(smartMonitorNoResponseAfter)) {
		t.Fatal("meaningful unanswered UDP/443 upload did not fail")
	}
	if !errors.Is(conn.outcomeError(), errSmartUDPNoResponse) {
		t.Fatalf("outcome error = %v", conn.outcomeError())
	}
}

func TestSmartPacketMonitorResponsePreventsFailureAndCommitsOnce(t *testing.T) {
	start := time.Unix(200, 0)
	commits := 0
	conn := &smartMonitoredPacketConn{
		metadata:        &C.Metadata{NetWork: C.UDP, DstPort: 443},
		firstWriteAt:    start,
		uploadBytes:     smartPacketMinUnansweredUpload,
		onFirstResponse: func(int64) { commits++ },
	}
	conn.observeResponse(100, start.Add(250*time.Millisecond))
	conn.observeResponse(100, start.Add(500*time.Millisecond))
	if conn.sample(start.Add(smartMonitorNoResponseAfter)) {
		t.Fatal("responded UDP path was treated as failed")
	}
	if commits != 1 {
		t.Fatalf("winner commits = %d, want 1", commits)
	}
	if got := conn.firstResponseLatency(); got != 250 {
		t.Fatalf("first response latency = %d, want 250", got)
	}
}

func TestSmartPacketMonitorDoesNotJudgeOtherUDPPorts(t *testing.T) {
	start := time.Unix(300, 0)
	conn := &smartMonitoredPacketConn{
		metadata:     &C.Metadata{NetWork: C.UDP, DstPort: 123},
		firstWriteAt: start,
		uploadBytes:  smartPacketMinUnansweredUpload,
	}
	if conn.sample(start.Add(10 * smartMonitorNoResponseAfter)) {
		t.Fatal("one-way non-443 UDP was treated as failed")
	}
}

func TestSmartPacketMonitorLateResponseDoesNotCommitFailedWinner(t *testing.T) {
	start := time.Unix(250, 0)
	commits := 0
	conn := &smartMonitoredPacketConn{
		metadata:        &C.Metadata{NetWork: C.UDP, DstPort: 443},
		firstWriteAt:    start,
		uploadBytes:     smartPacketMinUnansweredUpload,
		onFirstResponse: func(int64) { commits++ },
	}
	if !conn.sample(start.Add(smartMonitorNoResponseAfter)) {
		t.Fatal("unanswered UDP path did not fail")
	}
	conn.observeResponse(100, start.Add(smartMonitorNoResponseAfter+time.Millisecond))
	if commits != 0 {
		t.Fatalf("late response committed failed winner %d times", commits)
	}
}

func TestSmartPacketMonitorPublishesOnlyOneSelectionOutcome(t *testing.T) {
	start := time.Unix(275, 0)
	successes := 0
	failures := 0
	conn := &smartMonitoredPacketConn{
		metadata:        &C.Metadata{NetWork: C.UDP, DstPort: 443},
		firstWriteAt:    start,
		uploadBytes:     smartPacketMinUnansweredUpload,
		onFirstResponse: func(int64) { successes++ },
		onNoResponse:    func(error) { failures++ },
	}
	if !conn.sample(start.Add(smartMonitorNoResponseAfter)) {
		t.Fatal("unanswered UDP path did not fail")
	}
	conn.notifyNoResponse(errSmartUDPNoResponse)
	conn.notifyNoResponse(net.ErrClosed)
	conn.observeResponse(100, start.Add(smartMonitorNoResponseAfter+time.Millisecond))
	if successes != 0 || failures != 1 {
		t.Fatalf("selection outcomes success=%d failure=%d, want success=0 failure=1", successes, failures)
	}
}

func TestSmartPacketCooldownIsTargetScopedAndExpires(t *testing.T) {
	s := &Smart{}
	now := time.Unix(400, 0)
	s.coolDownActivePacketNode("node-a", "*.whatsapp.net", now)
	if !s.activePacketNodeCoolingDown("node-a", "*.whatsapp.net", now) {
		t.Fatal("UDP target cooldown was not active")
	}
	if s.activePacketNodeCoolingDown("node-b", "*.whatsapp.net", now) {
		t.Fatal("UDP cooldown leaked to another node")
	}
	if s.activePacketNodeCoolingDown("node-a", "*.facebook.com", now) {
		t.Fatal("UDP cooldown leaked to another target")
	}
	if s.activePacketNodeCoolingDown("node-a", "*.whatsapp.net", now.Add(smartMonitorCooldown)) {
		t.Fatal("expired UDP cooldown remained active")
	}
}
