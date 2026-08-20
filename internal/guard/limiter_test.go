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
		MaxTrackedIPs: 2, MaxTCPConnectionsPerIP: 1,
		ResponseBytesPerSecond: 100, ResponseBurstBytes: 20,
		IngressBytesPerSecond: 100, IngressBurstBytes: 20,
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
