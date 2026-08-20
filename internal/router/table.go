package router

import (
	"fmt"
	"sort"
	"strings"

	"github.com/TaJirax/CottenRouter/internal/config"
)

type routeEntry struct {
	domain     string
	backend    string
	tcpBackend string
	name       string
	verifyKey  []byte
	verifyMTU  int
}

type routeTable []routeEntry

func newRouteTable(routes []config.Route) (routeTable, error) {
	entries := make(routeTable, 0)
	for _, route := range routes {
		for _, domain := range route.Domains {
			entries = append(entries, routeEntry{
				domain: domain, backend: route.Backend, tcpBackend: route.TCPBackend, name: route.Name,
				verifyKey: route.VerifyKey, verifyMTU: verifyMTU(route),
			})
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no route domains configured")
	}
	sort.Slice(entries, func(i, j int) bool {
		return len(entries[i].domain) > len(entries[j].domain)
	})
	return entries, nil
}

func verifyMTU(route config.Route) int {
	if route.Verify == nil {
		return 0
	}
	return route.Verify.MTU
}

func (t routeTable) match(qname string) (routeEntry, bool) {
	qname = strings.ToLower(strings.TrimSuffix(qname, "."))
	for _, route := range t {
		if qname == route.domain || strings.HasSuffix(qname, "."+route.domain) {
			return route, true
		}
	}
	return routeEntry{}, false
}
