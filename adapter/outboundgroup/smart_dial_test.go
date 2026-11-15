package outboundgroup

import (
	"context"
	"errors"
	"testing"
	"time"

	C "github.com/metacubex/mihomo/constant"
)

type smartDialTestConn struct {
	C.Conn
	closed chan struct{}
}

func (c *smartDialTestConn) Close() error {
	close(c.closed)
	return nil
}

func TestSmartSoftHeadStartDoesNotCancelPreferredDial(t *testing.T) {
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart})}
	proxies := []C.Proxy{smartVideoTestProxy{name: "US"}, smartVideoTestProxy{name: "NL"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	backupStarted := make(chan struct{})
	backupOutcome := make(chan string, 1)
	preferred := &smartDialTestConn{closed: make(chan struct{})}
	backup := &smartDialTestConn{closed: make(chan struct{})}
	batches := []smartDialBatch{{end: 1, wait: 15 * time.Millisecond}, {end: 2}}
	winner, conn, _, err := s.raceDialContext(ctx, proxies, &C.Metadata{}, time.Now(), batches,
		func(ctx context.Context, proxy C.Proxy, _ *C.Metadata, _ time.Time) (C.Conn, int64, error) {
			if proxy.Name() == "US" {
				select {
				case <-backupStarted:
					return preferred, 0, ctx.Err()
				case <-ctx.Done():
					return nil, 0, ctx.Err()
				}
			}
			close(backupStarted)
			<-ctx.Done()
			backupOutcome <- smartDialOutcome(ctx, ctx.Err())
			return backup, 0, nil
		})
	if err != nil || winner.Name() != "US" || conn != preferred {
		t.Fatalf("preferred dial could not finish once the backup started: winner=%v, err=%v", winner, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	select {
	case outcome := <-backupOutcome:
		if outcome != "race-canceled" {
			t.Fatalf("loser outcome = %s", outcome)
		}
	case <-ctx.Done():
		t.Fatal("the losing dial was not canceled")
	}
	select {
	case <-backup.closed:
	case <-ctx.Done():
		t.Fatal("the losing socket was not closed")
	}
}

func TestSmartDialFailuresAdvanceTiersAndPreserveCauses(t *testing.T) {
	s := &Smart{GroupBase: NewGroupBase(GroupBaseOption{Name: t.Name(), Type: C.Smart})}
	proxies := []C.Proxy{smartVideoTestProxy{name: "US"}, smartVideoTestProxy{name: "JP"}, smartVideoTestProxy{name: "NL"}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	a, b, c := errors.New("refused"), errors.New("reset"), errors.New("unreachable")
	failures := map[string]error{"US": a, "JP": b, "NL": c}
	var launched []string
	batches := []smartDialBatch{{end: 1, wait: time.Hour}, {end: 2, wait: time.Hour}, {end: 3}}
	_, _, _, err := s.raceDialContext(ctx, proxies, &C.Metadata{}, time.Now(), batches,
		func(_ context.Context, proxy C.Proxy, _ *C.Metadata, _ time.Time) (C.Conn, int64, error) {
			launched = append(launched, proxy.Name())
			return nil, 0, failures[proxy.Name()]
		})
	if !errors.Is(err, a) || !errors.Is(err, b) || !errors.Is(err, c) {
		t.Fatalf("candidate failures were lost: %v", err)
	}
	if len(launched) != 3 || launched[0] != "US" || launched[1] != "JP" || launched[2] != "NL" {
		t.Fatalf("hard failure did not advance through tiers: %v", launched)
	}
}

func TestSmartHardDeadlineDoesNotCountAsNodeFailure(t *testing.T) {
	s := &Smart{}
	proxies := []C.Proxy{smartVideoTestProxy{name: "US"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var outcome string
	_, _, _, err := s.raceDialContext(ctx, proxies, &C.Metadata{}, time.Now(), nil,
		func(ctx context.Context, _ C.Proxy, _ *C.Metadata, _ time.Time) (C.Conn, int64, error) {
			<-ctx.Done()
			err := errors.New("transport returns an error at request deadline")
			outcome = smartDialOutcome(ctx, err)
			return nil, 0, err
		})
	if !errors.Is(err, context.DeadlineExceeded) || outcome != "request-deadline" || shouldRecordSmartDialFailure(ctx, err) {
		t.Fatalf("deadline classified incorrectly: %v, outcome=%q", err, outcome)
	}
	if shouldRecordSmartDialFailure(context.Background(), context.DeadlineExceeded) {
		t.Fatal("an adapter's own context deadline was recorded as a node failure")
	}
}
