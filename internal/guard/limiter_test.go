package guard

import (
	"net/netip"
	"testing"
	"time"

	"github.com/TaJirax/CottenRouter/internal/config"
)

func testLimits() config.Limits {
	return config.Limits{
		GlobalQueriesPerSecond: 10, GlobalQueryBurst: 4,
		PerIPQueriesPerSecond: 10, PerIPQueryBurst: 2,
		PerIPIngressBytesPerSecond: 100, PerIPIngressBurstBytes: 20,
		MaxTrackedIPs: 2, MaxTCPConnectionsPerIP: 1, MaxTLSConnectionsPerIP: 1,
		ResponseBytesPerSecond: 100, ResponseBurstBytes: 20,
		IngressBytesPerSecond: 100, IngressBurstBytes: 20,
	}
}

func TestSourceIngressLimitDoesNotDrainSharedDNSBudget(t *testing.T) {
	limits := testLimits()
	limits.PerIPIngressBurstBytes = 10
	limiter := New(limits)
	attacker := netip.MustParseAddr("192.0.2.50")
	victim := netip.MustParseAddr("192.0.2.60")
	if !limiter.AllowSourceProtocolIngress(attacker, "dns", 10) {
		t.Fatal("first source ingress burst should pass")
	}
	if limiter.AllowSourceProtocolIngress(attacker, "dns", 1) {
		t.Fatal("source exceeded its ingress byte burst")
	}
	if !limiter.AllowSourceProtocolIngress(victim, "dns", 10) {
		t.Fatal("source-limited attacker drained the shared DNS ingress budget")
	}
}

func TestAggregateTrafficCeilingBoundsAllClasses(t *testing.T) {
	limits := testLimits()
	limits.ResponseBurstBytes = 20
	limits.AggregateResponseBurstBytes = 40
	limits.AggregateResponseBytesPerSecond = 100
	limiter := New(limits)
	if !limiter.AllowProtocolResponse("naiveproxy", 20) || !limiter.AllowProtocolResponse("doh", 20) {
		t.Fatal("traffic inside aggregate response ceiling was rejected")
	}
	if limiter.AllowProtocolResponse("dns", 1) {
		t.Fatal("aggregate response ceiling did not bound combined protocol traffic")
	}
}

func TestProtocolTrafficBudgetsAreIsolated(t *testing.T) {
	limiter := New(testLimits())
	if !limiter.AllowProtocolResponse("naiveproxy", 20) || limiter.AllowProtocolResponse("naiveproxy", 1) {
		t.Fatal("relay response bucket was not exhausted as expected")
	}
	if !limiter.AllowProtocolResponse("doh", 20) || !limiter.AllowResponse(20) {
		t.Fatal("relay traffic consumed DoH or clear-DNS response capacity")
	}
	if !limiter.AllowProtocolIngress("stuntls", 20) || !limiter.AllowProtocolIngress("dot", 20) {
		t.Fatal("StunTLS traffic consumed DoT ingress capacity")
	}
}

func TestTLSConnectionClassesAreIsolatedPerIP(t *testing.T) {
	limiter := New(testLimits())
	ip := netip.MustParseAddr("192.0.2.40")
	if !limiter.AcquireTLS(ip, "naiveproxy") || limiter.AcquireTLS(ip, "naiveproxy") {
		t.Fatal("NaiveProxy per-IP connection cap was not enforced")
	}
	if !limiter.AcquireTLS(ip, "doh") {
		t.Fatal("NaiveProxy connection consumed the independent DoH allowance")
	}
	limiter.ReleaseTLS(ip, "naiveproxy")
	if !limiter.AcquireTLS(ip, "naiveproxy") {
		t.Fatal("released NaiveProxy slot was not reusable")
	}
}

func TestQueryBucketsAndRefill(t *testing.T) {
	limiter := New(testLimits())
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	limiter.global = bucket{tokens: 4, last: now}
	ip := netip.MustParseAddr("192.0.2.1")
	if !limiter.AllowQuery(ip) || !limiter.AllowQuery(ip) || limiter.AllowQuery(ip) {
		t.Fatal("per-IP burst was not enforced")
	}
	now = now.Add(time.Second)
	if !limiter.AllowQuery(ip) {
		t.Fatal("tokens did not refill")
	}
}

func TestTCPAndResponseLimits(t *testing.T) {
	limiter := New(testLimits())
	ip := netip.MustParseAddr("192.0.2.2")
	if !limiter.AcquireTCP(ip) || limiter.AcquireTCP(ip) {
		t.Fatal("per-IP TCP limit was not enforced")
	}
	limiter.ReleaseTCP(ip)
	if !limiter.AcquireTCP(ip) {
		t.Fatal("released TCP slot was not reusable")
	}
	if !limiter.AllowResponse(20) || limiter.AllowResponse(1) {
		t.Fatal("response-byte burst was not enforced")
	}
}

func TestTrustedResolverBypassesOnlyPerIPQueryBucket(t *testing.T) {
	limits := testLimits()
	limits.TrustedResolverCIDRs = []string{"192.0.2.0/24"}
	limits.GlobalQueryBurst = 3
	limiter := New(limits)
	ip := netip.MustParseAddr("192.0.2.53")
	if !limiter.AllowQuery(ip) || !limiter.AllowQuery(ip) || !limiter.AllowQuery(ip) || limiter.AllowQuery(ip) {
		t.Fatal("trusted resolver did not bypass per-IP limit or bypassed global limit")
	}
}

func TestFreshSourceTableCannotBeChurnedToResetBuckets(t *testing.T) {
	limits := testLimits()
	limits.MaxTrackedIPs = 1
	limiter := New(limits)
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	limiter.global = bucket{tokens: 100, last: now}
	first := netip.MustParseAddr("192.0.2.1")
	second := netip.MustParseAddr("192.0.2.2")
	if !limiter.AllowQuery(first) || limiter.AllowQuery(second) {
		t.Fatal("fresh tracked source was evicted by address churn")
	}
	now = now.Add(time.Minute)
	if !limiter.AllowQuery(second) {
		t.Fatal("idle source entry was not recyclable")
	}
}

func TestRejectedSourceCannotDrainGlobalQueryBucket(t *testing.T) {
	limits := testLimits()
	limits.GlobalQueryBurst = 4
	limits.PerIPQueryBurst = 1
	limiter := New(limits)
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	limiter.global = bucket{tokens: 4, last: now}
	attacker := netip.MustParseAddr("192.0.2.10")
	victim := netip.MustParseAddr("192.0.2.20")
	if !limiter.AllowQuery(attacker) {
		t.Fatal("first attacker query should fit its source bucket")
	}
	for range 3 {
		if limiter.AllowQuery(attacker) {
			t.Fatal("attacker exceeded its source bucket")
		}
	}
	if !limiter.AllowQuery(victim) {
		t.Fatal("source-limited traffic drained the shared query bucket")
	}
}

func TestTLSHandshakeFloodCannotDrainDNSQueryBuckets(t *testing.T) {
	limits := testLimits()
	limits.GlobalQueryBurst = 2
	limits.PerIPQueryBurst = 2
	limiter := New(limits)
	now := time.Unix(100, 0)
	limiter.now = func() time.Time { return now }
	limiter.global = bucket{tokens: 2, last: now}
	limiter.tlsHandshakeGlobal = bucket{tokens: 2, last: now}
	attacker := netip.MustParseAddr("192.0.2.70")
	victim := netip.MustParseAddr("192.0.2.80")
	if !limiter.AllowTLSHandshake(attacker) || !limiter.AllowTLSHandshake(attacker) {
		t.Fatal("TLS handshake traffic inside its bounded burst was rejected")
	}
	if limiter.AllowTLSHandshake(victim) {
		t.Fatal("distributed TLS handshakes exceeded the independent global CPU budget")
	}
	if !limiter.AllowQuery(victim) || !limiter.AllowQuery(victim) {
		t.Fatal("TLS handshake flood drained DNS query admission tokens")
	}
}
