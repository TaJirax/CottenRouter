package guard

import (
	"net/netip"
	"sync"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
)

type Limiter struct {
	mu        sync.Mutex
	limits    config.Limits
	global    bucket
	responses bucket
	ingress   bucket
	sources   map[netip.Addr]*source
	trusted   []netip.Prefix
	now       func() time.Time
}

type source struct {
	queries        bucket
	tcpConnections int
	lastSeen       time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func New(limits config.Limits) *Limiter {
	now := time.Now()
	limiter := &Limiter{
		limits:    limits,
		global:    bucket{tokens: float64(limits.GlobalQueryBurst), last: now},
		responses: bucket{tokens: float64(limits.ResponseBurstBytes), last: now},
		ingress:   bucket{tokens: float64(limits.IngressBurstBytes), last: now},
		sources:   make(map[netip.Addr]*source),
		now:       time.Now,
	}
	for _, value := range limits.TrustedResolverCIDRs {
		if prefix, err := netip.ParsePrefix(value); err == nil {
			limiter.trusted = append(limiter.trusted, prefix)
		}
	}
	return limiter
}

func (l *Limiter) AllowQuery(address netip.Addr) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !consume(&l.global, now, float64(l.limits.GlobalQueriesPerSecond), float64(l.limits.GlobalQueryBurst), 1) {
		return false
	}
	address = address.Unmap()
	if l.isTrusted(address) {
		return true
	}
	state := l.sourceFor(address, now)
	if state == nil {
		return false
	}
	state.lastSeen = now
	return consume(&state.queries, now, float64(l.limits.PerIPQueriesPerSecond), float64(l.limits.PerIPQueryBurst), 1)
}

func (l *Limiter) AllowResponse(size int) bool {
	if size <= 0 {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	return consume(&l.responses, now, float64(l.limits.ResponseBytesPerSecond), float64(l.limits.ResponseBurstBytes), float64(size))
}

func (l *Limiter) AllowIngress(size int) bool {
	if size <= 0 {
		return false
	}
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	return consume(&l.ingress, now, float64(l.limits.IngressBytesPerSecond), float64(l.limits.IngressBurstBytes), float64(size))
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

func (l *Limiter) Prune(maxIdle time.Duration) {
	cutoff := l.now().Add(-maxIdle)
	l.mu.Lock()
	defer l.mu.Unlock()
	for address, state := range l.sources {
		if state.tcpConnections == 0 && state.lastSeen.Before(cutoff) {
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
		var oldestAddress netip.Addr
		var oldest *source
		for candidate, state := range l.sources {
			if state.tcpConnections == 0 && (oldest == nil || state.lastSeen.Before(oldest.lastSeen)) {
				oldestAddress, oldest = candidate, state
			}
		}
		if oldest == nil || now.Sub(oldest.lastSeen) < time.Minute {
			return nil
		}
		delete(l.sources, oldestAddress)
	}
	state := &source{
		queries:  bucket{tokens: float64(l.limits.PerIPQueryBurst), last: now},
		lastSeen: now,
	}
	l.sources[address] = state
	return state
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
