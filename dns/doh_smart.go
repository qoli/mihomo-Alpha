package dns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/atomic"
	"github.com/metacubex/mihomo/log"
	"github.com/metacubex/mihomo/tunnel"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptrace"
	D "github.com/miekg/dns"
)

type doHProgressKey struct{}
type doHBackupKey struct{}

var doHRetryID atomic.Uint64

type doHProgress struct {
	mu      sync.Mutex
	changed chan struct{}
	route   tunnel.DoHRoute
	phase   string
}

func (p *doHProgress) observe(route tunnel.DoHRoute, phase string) {
	p.mu.Lock()
	// A speculative dial may finish after the transport has reused a
	// connection. GotConn is authoritative for an in-flight HTTP exchange.
	if p.phase != "doh" || phase != "tls" {
		p.route, p.phase = route, phase
	}
	p.mu.Unlock()
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

type doHBackupDial struct {
	ctx   context.Context
	route tunnel.DoHRoute
	id    uint64
}

func (doh *dnsOverHTTPS) dialHTTPS(ctx context.Context, network, addr string) (net.Conn, error) {
	if backup, ok := ctx.Value(doHBackupKey{}).(doHBackupDial); ok {
		// The HTTP transport detaches its dial context from the request.
		// This dial belongs to one query and must share its cancellation.
		return doh.dialer.DialDoHBackup(backup.ctx, network, addr, backup.route, backup.id)
	}

	conn, err := doh.dialer.DialContext(ctx, network, addr)
	if progress, ok := ctx.Value(doHProgressKey{}).(*doHProgress); ok && err == nil {
		progress.observe(doh.dialer.DoHRoute(conn), "tls")
	}
	return conn, err
}

type doHResult struct {
	msg *D.Msg
	err error
}

// exchangeWithSmartBackup reserves the latter half of the existing DNS
// deadline for an alternate path. Until then the original transport, including
// any pooled connection, handles the request on its own.
func (doh *dnsOverHTTPS) exchangeWithSmartBackup(ctx context.Context, client *http.Client, req *D.Msg) (*D.Msg, error, bool) {
	transport, tcp := client.Transport.(*http.Transport)
	if !tcp || doh.url.Scheme != "https" || !doh.dialer.CanRetryDoH() {
		msg, err := doh.exchangeHTTPS(ctx, client, req)
		return msg, err, false
	}

	start := time.Now()
	ctx, cancel := context.WithDeadline(ctx, start.Add(DefaultTimeout))
	defer cancel()
	deadline, _ := ctx.Deadline()
	timer := time.NewTimer(time.Until(start.Add(deadline.Sub(start) / 2)))
	defer timer.Stop()
	checkpoint := timer.C

	progress := &doHProgress{changed: make(chan struct{}, 1)}
	primaryCtx := context.WithValue(ctx, doHProgressKey{}, progress)
	primaryCtx = httptrace.WithClientTrace(primaryCtx, &httptrace.ClientTrace{
		GetConn: func(string) { progress.observe(tunnel.DoHRoute{}, "") },
		GotConn: func(info httptrace.GotConnInfo) {
			progress.observe(doh.dialer.DoHRoute(info.Conn), "doh")
		},
	})
	primaryCtx, cancelPrimary := context.WithCancel(primaryCtx)
	defer cancelPrimary()
	primary := make(chan doHResult, 1)
	go func() {
		msg, err := doh.exchangeHTTPS(primaryCtx, client, req)
		primary <- doHResult{msg, err}
	}()

	for {
		select {
		case result := <-primary:
			return result.msg, result.err, false
		case <-ctx.Done():
			return nil, ctx.Err(), false
		case <-checkpoint:
			checkpoint = nil
		case <-progress.changed:
		}
		if checkpoint != nil {
			continue
		}

		progress.mu.Lock()
		route, phase := progress.route, progress.phase
		progress.mu.Unlock()
		if !doh.dialer.CanRetryDoHRoute(route) {
			continue
		}
		select {
		case result := <-primary:
			return result.msg, result.err, false
		default:
		}
		if err := ctx.Err(); err != nil {
			return nil, err, false
		}

		id := doHRetryID.Add(1)
		log.Debugln("[DoH Smart] retry=%d phase=stalled stage=%s route=%s elapsed=%s", id, phase, route, time.Since(start))
		msg, err := doh.raceDoHBackup(ctx, client, transport, req, primary, cancelPrimary, route, id)
		return msg, err, true
	}
}

func (doh *dnsOverHTTPS) raceDoHBackup(ctx context.Context, old *http.Client, transport *http.Transport, req *D.Msg, primary <-chan doHResult, cancelPrimary context.CancelFunc, route tunnel.DoHRoute, id uint64) (*D.Msg, error) {
	backupCtx, cancelBackup := context.WithCancel(ctx)
	next := &http.Client{Transport: transport.Clone(), Timeout: old.Timeout}
	promoted := false
	defer func() {
		cancelBackup()
		if !promoted {
			_ = doh.closeClient(next)
		}
	}()
	backupResult := make(chan doHResult, 1)
	var backup <-chan doHResult = backupResult
	start := time.Now()
	go func() {
		msg, err := doh.exchangeHTTPS(context.WithValue(backupCtx, doHBackupKey{}, doHBackupDial{backupCtx, route, id}), next, req)
		backupResult <- doHResult{msg, err}
	}()

	var primaryErr, backupErr error
	for primary != nil || backup != nil {
		select {
		case r := <-primary:
			primaryErr = checkDoHRecoveryReply(req, r)
			if primaryErr == nil {
				return r.msg, nil
			}
			primary = nil
		case r := <-backup:
			backupErr = errors.Join(checkDoHRecoveryReply(req, r), ctx.Err())
			if backupErr == nil {
				log.Debugln("[DoH Smart] retry=%d phase=answered elapsed=%s", id, time.Since(start))
				cancelPrimary()
				doh.clientMu.Lock()
				if doh.client == old && doh.dialer.CanRetryDoHRoute(route) {
					doh.client = next
					promoted = true
				}
				doh.clientMu.Unlock()
				if promoted {
					// Other DNS requests may still be using the old HTTP/2
					// transport. Close only idle connections.
					old.CloseIdleConnections()
				}
				return r.msg, nil
			}
			backup = nil
		case <-ctx.Done():
			return nil, fmt.Errorf("DoH Smart retry after stalling on %s: %w", route, errors.Join(ctx.Err(), primaryErr, backupErr))
		}
	}
	return nil, fmt.Errorf("DoH Smart retry after stalling on %s: %w", route, errors.Join(primaryErr, backupErr))
}

func checkDoHRecoveryReply(req *D.Msg, result doHResult) error {
	if result.err != nil {
		return result.err
	}
	msg := result.msg
	if msg == nil || !msg.Response || len(msg.Question) != len(req.Question) {
		return errors.New("invalid DoH reply")
	}
	for i, question := range msg.Question {
		want := req.Question[i]
		if question.Qtype != want.Qtype || question.Qclass != want.Qclass || !strings.EqualFold(question.Name, want.Name) {
			return errors.New("DoH reply question mismatch")
		}
	}
	if msg.Rcode == D.RcodeServerFailure || msg.Rcode == D.RcodeRefused {
		return fmt.Errorf("DoH reply: %s", D.RcodeToString[msg.Rcode])
	}
	return nil
}
