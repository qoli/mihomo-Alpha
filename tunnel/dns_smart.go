package tunnel

import (
	"context"
	"fmt"
	"net"
	"strings"

	C "github.com/metacubex/mihomo/constant"
)

type dnsSmartGroup interface {
	C.ProxyAdapter
	CanRetryDoH() bool
	DialDoHBackup(ctx context.Context, metadata *C.Metadata, excluded string, trace uint64, dial func(context.Context, C.Proxy, *C.Metadata) (C.Conn, error)) (C.Conn, error)
}

// DoHRoute identifies the Smart member used by an actual TCP connection.
type DoHRoute struct {
	group dnsSmartGroup
	node  string
}

func (r DoHRoute) String() string {
	if r.group == nil {
		return ""
	}
	return fmt.Sprintf("%s[%s]", r.group.Name(), r.node)
}

// smartDoHGroup follows selected paths only. A retry must never change the
// selector's choice or replace a route chosen by a rule or balancing group.
func smartDoHGroup(proxy C.ProxyAdapter, touch bool) (dnsSmartGroup, []C.ProxyAdapter) {
	var selectors []C.ProxyAdapter
	for proxy != nil {
		if p, ok := proxy.(C.Proxy); ok {
			proxy = p.Adapter()
		}
		if group, ok := proxy.(dnsSmartGroup); ok {
			return group, selectors
		}
		if proxy.Type() != C.Selector {
			break
		}
		selectors = append(selectors, proxy)
		proxy = proxy.Unwrap(nil, touch)
	}
	return nil, nil
}

func (d *DNSDialer) explicitProxy() C.ProxyAdapter {
	if d.proxyAdapter != nil {
		return d.proxyAdapter
	}
	if d.proxyName == "" || d.proxyName == DnsRespectRules {
		return nil
	}
	if proxy, ok := Proxies()[d.proxyName]; ok {
		return proxy
	}
	return nil
}

func (d *DNSDialer) CanRetryDoH() bool {
	group, _ := smartDoHGroup(d.explicitProxy(), false)
	return group != nil && group.CanRetryDoH()
}

func (d *DNSDialer) CanRetryDoHRoute(route DoHRoute) bool {
	if route.group == nil {
		return false
	}
	group, _ := smartDoHGroup(d.explicitProxy(), false)
	return group == route.group && group.CanRetryDoH()
}

func (d *DNSDialer) DoHRoute(conn net.Conn) DoHRoute {
	if tlsConn, ok := conn.(interface{ NetConn() net.Conn }); ok {
		conn = tlsConn.NetConn()
	}
	tracked, ok := conn.(C.Conn)
	if !ok {
		return DoHRoute{}
	}
	group, _ := smartDoHGroup(d.explicitProxy(), false)
	if group == nil {
		return DoHRoute{}
	}
	chain := tracked.Chains()
	for i := 1; i < len(chain); i++ {
		if chain[i] == group.Name() {
			return DoHRoute{group: group, node: chain[i-1]}
		}
	}
	return DoHRoute{}
}

func (d *DNSDialer) DialDoHBackup(ctx context.Context, network, addr string, failed DoHRoute, trace uint64) (net.Conn, error) {
	if failed.group == nil || !strings.Contains(network, "tcp") {
		return nil, fmt.Errorf("DoH backup requires a Smart TCP route")
	}
	return d.dialContext(ctx, network, addr, failed, trace)
}

func (d *DNSDialer) dialDoHBackup(ctx context.Context, proxy C.ProxyAdapter, metadata *C.Metadata, failed DoHRoute, trace uint64) (C.Conn, error) {
	group, selectors := smartDoHGroup(proxy, true)
	if group == nil || group != failed.group {
		return nil, fmt.Errorf("DoH Smart route changed after using %s", failed)
	}
	conn, err := group.DialDoHBackup(ctx, metadata, failed.node, trace, func(ctx context.Context, candidate C.Proxy, target *C.Metadata) (C.Conn, error) {
		if candidate.IsL3Protocol(target) {
			// Parallel Smart dials share the selection metadata. Only the L3
			// candidate needs a copy with the bootstrap IP and no hostname.
			resolved := *target
			if err := d.resolveDNSDestination(ctx, &resolved); err != nil {
				return nil, err
			}
			target = &resolved
		}
		return candidate.DialContext(ctx, target)
	})
	if err != nil {
		return nil, err
	}
	for i := len(selectors) - 1; i >= 0; i-- {
		conn.AppendToChains(selectors[i])
	}
	return conn, nil
}
