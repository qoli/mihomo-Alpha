package outboundgroup

import (
	"context"
	"errors"
	"sort"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"
)

var errSmartDialRaceWon = errors.New("another Smart candidate won the dial race")

type smartDialIDKey struct{}

func smartDialID(ctx context.Context) uint64 {
	id, _ := ctx.Value(smartDialIDKey{}).(uint64)
	return id
}

type smartDialFunc func(context.Context, C.Proxy, *C.Metadata, time.Time) (C.Conn, int64, error)

type smartDialBatch struct {
	end  int
	wait time.Duration
}

type dialResult struct {
	proxyIndex  int
	conn        C.Conn
	connectTime int64
	err         error
}

func smartDialOutcome(ctx context.Context, err error) string {
	switch {
	case errors.Is(context.Cause(ctx), errSmartDialRaceWon):
		return "race-canceled"
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "request-deadline"
	case errors.Is(ctx.Err(), context.Canceled), errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "attempt-deadline"
	case err == nil:
		return "connected"
	case tunnel.ShouldStopRetry(err):
		return "request-error"
	default:
		return "failed"
	}
}

func (s *Smart) singleDialContext(ctx context.Context, proxy C.Proxy, metadata *C.Metadata, start time.Time) (C.Conn, int64, error) {
	id := smartDialID(ctx)
	if id != 0 {
		log.Debugln("[Smart] Dial attempt: group=[%s] target=[%s] node=[%s] priority=%.2f dial=%d",
			s.Name(), metadata.SmartTarget, proxy.Name(), s.getPriorityFactor(proxy.Name()), id)
	}
	conn, err := proxy.DialContext(ctx, metadata)
	connectTime := time.Since(start).Milliseconds()
	outcome := smartDialOutcome(ctx, err)
	if id != 0 {
		log.Debugln("[Smart] Dial outcome: group=[%s] target=[%s] node=[%s] outcome=%s elapsed-ms=%d dial=%d",
			s.Name(), metadata.SmartTarget, proxy.Name(), outcome, connectTime, id)
	}
	if outcome == "failed" {
		go s.recordConnectionStats(metadata.Clone(), proxy, connectTime, 0, 0, 0, 0, 0, 0, nil, err)
	}
	return conn, connectTime, err
}

func (s *Smart) preferredDialOrder(ctx context.Context, metadata *C.Metadata, selection smartDialSelection) ([]C.Proxy, []smartDialBatch) {
	proxies := selection.proxies
	if selection.flight == nil || len(proxies) < 2 || len(selection.eligiblePriority) == 0 {
		return proxies, nil
	}
	ranked := append([]C.Proxy(nil), proxies...)
	factor := func(p C.Proxy) float64 { return selection.eligiblePriority[p.Name()] }
	sort.SliceStable(ranked, func(i, j int) bool { return factor(ranked[i]) > factor(ranked[j]) })

	var batches []smartDialBatch
	for i := 1; i < len(ranked); i++ {
		if factor(ranked[i-1]) != factor(ranked[i]) {
			batches = append(batches, smartDialBatch{end: i})
		}
	}
	if len(batches) == 0 {
		return proxies, nil
	}

	begin := 0
	for i := range batches {
		var estimate int64
		for _, p := range ranked[begin:batches[i].end] {
			history := s.getHistoryConnectStats(metadata, p)
			if history <= 0 {
				if delay := p.LastDelayForTestUrl(s.testUrl); delay != 0 && delay != 65535 {
					history = int64(delay)
				}
			}
			if history > estimate {
				estimate = history
			}
		}
		batches[i].wait = time.Duration(float64(estimate)*connectThreshold) * time.Millisecond
		begin = batches[i].end
	}
	deadline, _ := ctx.Deadline()
	// All head starts together leave at least half of the remaining request
	// budget for the final tier. Earlier attempts continue until the same
	// deadline or until another candidate succeeds.
	maxWait := time.Until(deadline) / 2 / time.Duration(len(batches))
	if maxWait <= 0 {
		return proxies, nil
	}
	for i := range batches {
		if batches[i].wait > maxWait {
			batches[i].wait = maxWait
		}
	}
	return ranked, append(batches, smartDialBatch{end: len(ranked)})
}

func (s *Smart) ParallelDialContext(ctx context.Context, proxies []C.Proxy, metadata *C.Metadata, start time.Time, dial smartDialFunc) (C.Proxy, C.Conn, int64, error) {
	return s.raceDialContext(ctx, proxies, metadata, start, nil, dial)
}

func (s *Smart) raceDialContext(ctx context.Context, proxies []C.Proxy, metadata *C.Metadata, start time.Time, batches []smartDialBatch, dial smartDialFunc) (C.Proxy, C.Conn, int64, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, 0, err
	}
	if len(proxies) == 0 {
		return nil, nil, 0, errSmartSelectionMissingWinner
	}
	if len(proxies) == 1 {
		conn, elapsed, err := dial(ctx, proxies[0], metadata, start)
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		if err != nil {
			if conn != nil {
				_ = conn.Close()
			}
			return nil, nil, 0, err
		}
		return proxies[0], conn, elapsed, nil
	}

	n := len(proxies)
	childCtx, cancel := context.WithCancelCause(ctx)
	results := make(chan dialResult, n)
	launched, received := 0, 0
	launch := func(until int, startedAt time.Time) {
		for launched < until {
			index := launched
			launched++
			target := metadata.Clone()
			go func() {
				conn, elapsed, err := dial(childCtx, proxies[index], target, startedAt)
				results <- dialResult{proxyIndex: index, conn: conn, connectTime: elapsed, err: err}
			}()
		}
	}
	defer func() {
		cancel(nil)
		if pending := launched - received; pending > 0 {
			go func() {
				for i := 0; i < pending; i++ {
					if result := <-results; result.conn != nil {
						_ = result.conn.Close()
					}
				}
			}()
		}
	}()

	if len(batches) == 0 {
		batches = []smartDialBatch{{end: n}}
	}
	id := smartDialID(ctx)
	var hedge *time.Timer
	var hedgeReady <-chan time.Time
	defer func() {
		if hedge != nil {
			hedge.Stop()
		}
	}()
	next := 0
	launchNext := func(startedAt time.Time) {
		if hedge != nil {
			hedge.Stop()
			hedge, hedgeReady = nil, nil
		}
		for next < len(batches) {
			batch := batches[next]
			count := batch.end - launched
			launch(batch.end, startedAt)
			next++
			if batch.wait > 0 {
				hedge = time.NewTimer(batch.wait)
				hedgeReady = hedge.C
				log.Debugln("[Smart] Dial preferred cohort: group=[%s] target=[%s] preferred=%d deferred=%d head-start=%s dial=%d",
					s.Name(), metadata.SmartTarget, count, n-launched, batch.wait, id)
				break
			}
		}
	}
	launchNext(start)

	errs := make([]error, 0, n)
	for received < n {
		select {
		case <-ctx.Done():
			return nil, nil, 0, ctx.Err()
		case <-hedgeReady:
			if ctx.Err() != nil {
				return nil, nil, 0, ctx.Err()
			}
			log.Debugln("[Smart] Dial deferred candidates: group=[%s] target=[%s] reason=head-start-elapsed dial=%d", s.Name(), metadata.SmartTarget, id)
			launchNext(time.Now())
		case result := <-results:
			received++
			if result.err == nil && ctx.Err() == nil {
				cancel(errSmartDialRaceWon)
				return proxies[result.proxyIndex], result.conn, result.connectTime, nil
			}
			if result.conn != nil {
				_ = result.conn.Close()
			}
			if ctx.Err() != nil {
				return nil, nil, 0, ctx.Err()
			}
			errs = append(errs, result.err)
			if received == launched && launched < n {
				log.Debugln("[Smart] Dial deferred candidates: group=[%s] target=[%s] reason=preferred-finished dial=%d", s.Name(), metadata.SmartTarget, id)
				launchNext(time.Now())
			}
		}
	}
	return nil, nil, 0, errors.Join(errs...)
}
