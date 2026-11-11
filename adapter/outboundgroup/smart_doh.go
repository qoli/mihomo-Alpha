package outboundgroup

import (
	"context"
	"fmt"
	"time"

	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"
)

func (s *Smart) CanRetryDoH() bool {
	return s.selected.Load() == "" && len(s.GetProxies(false)) > 1
}

func (s *Smart) DialDoHBackup(ctx context.Context, metadata *C.Metadata, excluded string, trace uint64, dial func(context.Context, C.Proxy, *C.Metadata) (C.Conn, error)) (C.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.selected.Load() != "" {
		return nil, fmt.Errorf("Smart %q has a manually selected member", s.Name())
	}

	start := time.Now()
	all := s.GetProxies(true)
	alternatives := make([]C.Proxy, 0, len(all))
	for _, p := range all {
		if p.Name() == excluded {
			continue
		}
		switch p.Type() {
		case C.Direct, C.Compatible, C.Reject, C.RejectDrop, C.Pass:
			continue
		}
		alternatives = append(alternatives, p)
	}
	proxies, _, state := s.selectProxiesWithState(metadata, alternatives, smartSelectionOptions{fresh: true, strictHealth: true})
	if state.fixed || len(proxies) == 0 {
		return nil, fmt.Errorf("Smart %q has no eligible DoH backup for %q", s.Name(), excluded)
	}
	log.Debugln("[DoH Smart] retry=%d phase=reselected group=%q excluded=%q candidates=%d elapsed=%s", trace, s.Name(), excluded, len(proxies), time.Since(start))

	start = time.Now()
	p, conn, connectTime, err := s.ParallelDialContext(ctx, proxies, metadata, start, func(ctx context.Context, proxy C.Proxy, target *C.Metadata, start time.Time) (C.Conn, int64, error) {
		conn, err := dial(ctx, proxy, target)
		return conn, time.Since(start).Milliseconds(), err
	})
	if err != nil {
		return nil, fmt.Errorf("Smart %q DoH backup dial: %w", s.Name(), err)
	}
	log.Debugln("[DoH Smart] retry=%d phase=dialed group=%q node=%q elapsed=%s", trace, s.Name(), p.Name(), time.Since(start))
	return s.WrapConnWithMetric(conn, p, metadata, connectTime), nil
}
