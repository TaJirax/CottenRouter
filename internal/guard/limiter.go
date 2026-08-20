package guard

import (
	"net/netip"
	"sync"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
)

type Limiter struct {
	mu                 sync.Mutex
	limits             config.Limits
	global             bucket
	tlsHandshakeGlobal bucket
	aggregateResponse  bucket
	aggregateIngress   bucket
	traffic            map[string]*trafficBuckets
	sources            map[netip.Addr]*source
	trusted            []netip.Prefix
	now                func() time.Time
	lastRecycle        time.Time
}

type source struct {
	queries        bucket
	tlsHandshakes  bucket
	ingress        bucket
	tcpConnections int
	tlsConnections map[string]int
	lastSeen       time.Time
}

type trafficBuckets struct {
	responses bucket
	ingress   bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func New(limits config.Limits) *Limiter {
	now := time.Now()
	if limits.AggregateResponseBytesPerSecond == 0 {
		limits.AggregateResponseBytesPerSecond = 6 * limits.ResponseBytesPerSecond
	}
	if limits.AggregateResponseBurstBytes == 0 {
		limits.AggregateResponseBurstBytes = 6 * limits.ResponseBurstBytes
	}
	if limits.AggregateIngressBytesPerSecond == 0 {
		limits.AggregateIngressBytesPerSecond = 6 * limits.IngressBytesPerSecond
	}
	if limits.AggregateIngressBurstBytes == 0 {
		limits.AggregateIngressBurstBytes = 6 * limits.IngressBurstBytes
	}
	limiter := &Limiter{
		limits:             limits,
		global:             bucket{tokens: float64(limits.GlobalQueryBurst), last: now},
		tlsHandshakeGlobal: bucket{tokens: float64(limits.GlobalQueryBurst), last: now},
		aggregateResponse:  bucket{tokens: float64(limits.AggregateResponseBurstBytes), last: now},
		aggregateIngress:   bucket{tokens: float64(limits.AggregateIngressBurstBytes), last: now},
		traffic:            make(map[string]*trafficBuckets),
		sources:            make(map[netip.Addr]*source),
		now:                time.Now,
	}
	for _, value := range limits.TrustedResolverCIDRs {
		if prefix, err := netip.ParsePrefix(value); err == nil {
			limiter.trusted = append(limiter.trusted, prefix)
		}
	}
	return limiter
}

// AllowTLSHandshake bounds TLS ClientHello parsing with independent global
// and per-source token buckets. A distributed TLS connection flood therefore
// remains CPU-bounded without spending admission tokens reserved for DNS.
func (l *Limiter) AllowTLSHandshake(address netip.Addr) bool {
	now := l.now()
	address = address.Unmap()
	if !address.IsValid() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isTrusted(address) {
		return consume(&l.tlsHandshakeGlobal, now, float64(l.limits.GlobalQueriesPerSecond), float64(l.limits.GlobalQueryBurst), 1)
	}
	state := l.sourceFor(address, now)
	if state == nil {
		return false
	}
	state.lastSeen = now
	if !consume(&state.tlsHandshakes, now, float64(l.limits.PerIPQueriesPerSecond), float64(l.limits.PerIPQueryBurst), 1) {
		return false
	}
	return consume(&l.tlsHandshakeGlobal, now, float64(l.limits.GlobalQueriesPerSecond), float64(l.limits.GlobalQueryBurst), 1)
}

func (l *Limiter) AllowQuery(address netip.Addr) bool {
	now := l.now()
	address = address.Unmap()
	if !address.IsValid() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isTrusted(address) {
		return consume(&l.global, now, float64(l.limits.GlobalQueriesPerSecond), float64(l.limits.GlobalQueryBurst), 1)
	}
	state := l.sourceFor(address, now)
	if state == nil {
		return false
	}
	state.lastSeen = now
	// Consume the source bucket first. A single abusive address must not drain
	// the shared bucket with requests that its own rate limit already rejects.
	if !consume(&state.queries, now, float64(l.limits.PerIPQueriesPerSecond), float64(l.limits.PerIPQueryBurst), 1) {
		return false
	}
	return consume(&l.global, now, float64(l.limits.GlobalQueriesPerSecond), float64(l.limits.GlobalQueryBurst), 1)
}

// AllowQueryIngress performs DNS query-count and ingress-byte admission under
// one lock. Besides keeping both decisions consistent, this removes a second
// mutex/map lookup from the UDP/TCP DNS hot path.
func (l *Limiter) AllowQueryIngress(address netip.Addr, size int) bool {
	if size <= 0 {
		return false
	}
	now := l.now()
	address = address.Unmap()
	if !address.IsValid() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.isTrusted(address) {
		if !consume(&l.global, now, float64(l.limits.GlobalQueriesPerSecond), float64(l.limits.GlobalQueryBurst), 1) {
			return false
		}
		return l.consumeProtocolIngress("dns", size, now)
	}
	state := l.sourceFor(address, now)
	if state == nil {
		return false
	}
	state.lastSeen = now
	if !consume(&state.queries, now, float64(l.limits.PerIPQueriesPerSecond), float64(l.limits.PerIPQueryBurst), 1) {
		return false
	}
	if !consume(&state.ingress, now, float64(l.limits.PerIPIngressBytesPerSecond), float64(l.limits.PerIPIngressBurstBytes), float64(size)) {
		return false
	}
	if !consume(&l.global, now, float64(l.limits.GlobalQueriesPerSecond), float64(l.limits.GlobalQueryBurst), 1) {
		return false
	}
	return l.consumeProtocolIngress("dns", size, now)
}

func (l *Limiter) AllowResponse(size int) bool {
	return l.AllowProtocolResponse("dns", size)
}

// AllowProtocolResponse applies an independent byte budget to each protocol
// class. A bulk relay (for example NaiveProxy) therefore cannot consume the
// response allowance reserved for DNS, DoH, DoT, or another relay class.
func (l *Limiter) AllowProtocolResponse(protocol string, size int) bool {
	if size <= 0 {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	class := l.trafficFor(protocol, now)
	if !consume(&class.responses, now, float64(l.limits.ResponseBytesPerSecond), float64(l.limits.ResponseBurstBytes), float64(size)) {
		return false
	}
	return consume(&l.aggregateResponse, now, float64(l.limits.AggregateResponseBytesPerSecond), float64(l.limits.AggregateResponseBurstBytes), float64(size))
}

func (l *Limiter) AllowIngress(size int) bool {
	return l.AllowProtocolIngress("dns", size)
}

// AllowProtocolIngress is the ingress counterpart of AllowProtocolResponse.
func (l *Limiter) AllowProtocolIngress(protocol string, size int) bool {
	if size <= 0 {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.consumeProtocolIngress(protocol, size, now)
}

// AllowSourceProtocolIngress applies the source byte bucket before shared
// protocol/aggregate buckets. Oversized traffic from one address therefore
// cannot spend tokens reserved for other clients. Trusted recursive resolvers
// bypass only this source bucket, matching AllowQuery's behavior.
func (l *Limiter) AllowSourceProtocolIngress(address netip.Addr, protocol string, size int) bool {
	if size <= 0 {
		return false
	}
	now := l.now()
	address = address.Unmap()
	if !address.IsValid() {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.isTrusted(address) {
		state := l.sourceFor(address, now)
		if state == nil {
			return false
		}
		state.lastSeen = now
		if !consume(&state.ingress, now, float64(l.limits.PerIPIngressBytesPerSecond), float64(l.limits.PerIPIngressBurstBytes), float64(size)) {
			return false
		}
	}
	return l.consumeProtocolIngress(protocol, size, now)
}

func (l *Limiter) AcquireTCP(address netip.Addr) bool {
	now := l.now()
	address = address.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.sourceFor(address, now)
	if state == nil || state.tcpConnections >= l.limits.MaxTCPConnectionsPerIP {
		return false
	}
	state.tcpConnections++
	state.lastSeen = now
	return true
}

func (l *Limiter) ReleaseTCP(address netip.Addr) {
	address = address.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	if state := l.sources[address]; state != nil {
		if state.tcpConnections > 0 {
			state.tcpConnections--
		}
		state.lastSeen = l.now()
	}
}

// AcquireTLS limits each protocol class independently. This prevents
// long-lived proxy streams from consuming the DoH/DoT connection allowance.
func (l *Limiter) AcquireTLS(address netip.Addr, protocol string) bool {
	now := l.now()
	address = address.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	state := l.sourceFor(address, now)
	if state == nil {
		return false
	}
	class := protocolClass(protocol)
	if state.tlsConnections == nil {
		state.tlsConnections = make(map[string]int)
	}
	if state.tlsConnections[class] >= l.limits.MaxTLSConnectionsPerIP {
		return false
	}
	state.tlsConnections[class]++
	state.lastSeen = now
	return true
}

func (l *Limiter) ReleaseTLS(address netip.Addr, protocol string) {
	address = address.Unmap()
	l.mu.Lock()
	defer l.mu.Unlock()
	if state := l.sources[address]; state != nil {
		class := protocolClass(protocol)
		if state.tlsConnections[class] > 0 {
			state.tlsConnections[class]--
		}
		state.lastSeen = l.now()
	}
}

func (l *Limiter) Prune(maxIdle time.Duration) {
	cutoff := l.now().Add(-maxIdle)
	l.mu.Lock()
	defer l.mu.Unlock()
	for address, state := range l.sources {
		if state.tcpConnections == 0 && tlsConnectionCount(state) == 0 && state.lastSeen.Before(cutoff) {
			delete(l.sources, address)
		}
	}
}

func (l *Limiter) sourceFor(address netip.Addr, now time.Time) *source {
	if !address.IsValid() {
		return nil
	}
	if state := l.sources[address]; state != nil {
		return state
	}
	if len(l.sources) >= l.limits.MaxTrackedIPs {
		// Do not let a rotating/spoofed source flood continuously reset its
		// per-IP token bucket by evicting fresh entries. Only recycle a source
		// that has been idle long enough; otherwise the new source is rejected.
		// At capacity, a spoofed source must not force an O(n) map scan for
		// every packet while holding the limiter mutex. Recycle at most once
		// per second; periodic pruning handles the normal idle case.
		if !l.lastRecycle.IsZero() && now.Sub(l.lastRecycle) < time.Second {
			return nil
		}
		l.lastRecycle = now
		var oldestAddress netip.Addr
		var oldest *source
		for candidate, state := range l.sources {
			if state.tcpConnections == 0 && tlsConnectionCount(state) == 0 && (oldest == nil || state.lastSeen.Before(oldest.lastSeen)) {
				oldestAddress, oldest = candidate, state
			}
		}
		if oldest == nil || now.Sub(oldest.lastSeen) < time.Minute {
			return nil
		}
		delete(l.sources, oldestAddress)
	}
	state := &source{
		queries:       bucket{tokens: float64(l.limits.PerIPQueryBurst), last: now},
		tlsHandshakes: bucket{tokens: float64(l.limits.PerIPQueryBurst), last: now},
		ingress:       bucket{tokens: float64(l.limits.PerIPIngressBurstBytes), last: now},
		lastSeen:      now,
	}
	l.sources[address] = state
	return state
}

func (l *Limiter) trafficFor(protocol string, now time.Time) *trafficBuckets {
	class := protocolClass(protocol)
	if value := l.traffic[class]; value != nil {
		return value
	}
	value := &trafficBuckets{
		responses: bucket{tokens: float64(l.limits.ResponseBurstBytes), last: now},
		ingress:   bucket{tokens: float64(l.limits.IngressBurstBytes), last: now},
	}
	l.traffic[class] = value
	return value
}

func (l *Limiter) consumeProtocolIngress(protocol string, size int, now time.Time) bool {
	class := l.trafficFor(protocol, now)
	if !consume(&class.ingress, now, float64(l.limits.IngressBytesPerSecond), float64(l.limits.IngressBurstBytes), float64(size)) {
		return false
	}
	return consume(&l.aggregateIngress, now, float64(l.limits.AggregateIngressBytesPerSecond), float64(l.limits.AggregateIngressBurstBytes), float64(size))
}

// Keep the number of remotely selectable resource buckets fixed even when
// route/listener names are user-controlled.
func protocolClass(protocol string) string {
	switch protocol {
	case "dns", "doh", "dot", "naiveproxy", "stuntls", "handshake":
		return protocol
	default:
		return "tls/other"
	}
}

func tlsConnectionCount(state *source) int {
	total := 0
	for _, count := range state.tlsConnections {
		total += count
	}
	return total
}

func (l *Limiter) isTrusted(address netip.Addr) bool {
	for _, prefix := range l.trusted {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

func consume(value *bucket, now time.Time, rate, burst, cost float64) bool {
	if elapsed := now.Sub(value.last).Seconds(); elapsed > 0 {
		value.tokens += elapsed * rate
		if value.tokens > burst {
			value.tokens = burst
		}
		value.last = now
	}
	if value.tokens < cost {
		return false
	}
	value.tokens -= cost
	return true
}
