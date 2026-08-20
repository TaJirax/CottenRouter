package telemetry

import (
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Registry struct {
	started time.Time
	items   sync.Map
	dropped atomic.Uint64
	limited atomic.Uint64
}

type counter struct {
	queries, bytesIn, bytesOut, sessions, errors atomic.Uint64
	active                                       atomic.Int64
}

type Snapshot struct {
	StartedAt   string   `json:"started_at"`
	UptimeSec   int64    `json:"uptime_seconds"`
	Dropped     uint64   `json:"dropped"`
	Limited     uint64   `json:"rate_limited"`
	MemoryBytes uint64   `json:"memory_bytes"`
	Goroutines  int      `json:"goroutines"`
	Protocols   []Metric `json:"protocols"`
}

type Metric struct {
	Protocol string `json:"protocol"`
	Route    string `json:"route,omitempty"`
	Queries  uint64 `json:"queries"`
	BytesIn  uint64 `json:"bytes_in"`
	BytesOut uint64 `json:"bytes_out"`
	Sessions uint64 `json:"sessions_total"`
	Active   int64  `json:"sessions_active"`
	Errors   uint64 `json:"errors"`
}

func New() *Registry { return &Registry{started: time.Now()} }

// Ensure exposes configured protocols before their first packet arrives.
func (r *Registry) Ensure(protocol, route string) { r.get(protocol, route) }

func (r *Registry) get(protocol, route string) *counter {
	value, _ := r.items.LoadOrStore(protocol+"\x00"+route, &counter{})
	return value.(*counter)
}

func (r *Registry) Query(protocol, route string, size int) {
	c := r.get(protocol, route)
	c.queries.Add(1)
	if size > 0 {
		c.bytesIn.Add(uint64(size))
	}
}
func (r *Registry) In(protocol, route string, size int) {
	if size > 0 {
		r.get(protocol, route).bytesIn.Add(uint64(size))
	}
}
func (r *Registry) Out(protocol, route string, size int) {
	if size > 0 {
		r.get(protocol, route).bytesOut.Add(uint64(size))
	}
}
func (r *Registry) SessionOpen(protocol, route string) {
	c := r.get(protocol, route)
	c.sessions.Add(1)
	c.active.Add(1)
}
func (r *Registry) SessionClose(protocol, route string) { r.get(protocol, route).active.Add(-1) }
func (r *Registry) Error(protocol, route string)        { r.get(protocol, route).errors.Add(1) }
func (r *Registry) Drop(limited bool) {
	r.dropped.Add(1)
	if limited {
		r.limited.Add(1)
	}
}

func (r *Registry) Snapshot() Snapshot {
	now := time.Now()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	result := Snapshot{StartedAt: r.started.UTC().Format(time.RFC3339), UptimeSec: int64(now.Sub(r.started).Seconds()), Dropped: r.dropped.Load(), Limited: r.limited.Load(), MemoryBytes: memory.Alloc, Goroutines: runtime.NumGoroutine()}
	r.items.Range(func(key, value any) bool {
		parts := strings.SplitN(key.(string), "\x00", 2)
		c := value.(*counter)
		result.Protocols = append(result.Protocols, Metric{Protocol: parts[0], Route: parts[1], Queries: c.queries.Load(), BytesIn: c.bytesIn.Load(), BytesOut: c.bytesOut.Load(), Sessions: c.sessions.Load(), Active: c.active.Load(), Errors: c.errors.Load()})
		return true
	})
	sort.Slice(result.Protocols, func(i, j int) bool {
		if result.Protocols[i].Protocol == result.Protocols[j].Protocol {
			return result.Protocols[i].Route < result.Protocols[j].Route
		}
		return result.Protocols[i].Protocol < result.Protocols[j].Protocol
	})
	return result
}
