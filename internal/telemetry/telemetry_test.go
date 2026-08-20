package telemetry

import "testing"

func TestSnapshotTracksTrafficAndSessions(t *testing.T) {
	r := New()
	r.Query("dns/udp", "cotten", 100)
	r.Out("dns/udp", "cotten", 200)
	r.SessionOpen("doh", "cotten")
	r.In("doh", "cotten", 50)
	r.SessionClose("doh", "cotten")
	r.Drop(true)
	s := r.Snapshot()
	if s.Dropped != 1 || s.Limited != 1 || len(s.Protocols) != 2 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
	if s.Protocols[0].BytesIn+s.Protocols[1].BytesIn != 150 {
		t.Fatalf("input bytes not tracked: %+v", s.Protocols)
	}
}
