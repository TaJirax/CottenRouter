//go:build race

package router

// raceEnabled reports whether the race detector instruments this build. The
// detector adds a per-memory-access cost that scales with how much work a path
// does, so it cannot be used to compare the router against a one-hop baseline.
const raceEnabled = true
