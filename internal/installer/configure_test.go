package installer

import (
	"strings"
	"testing"
)

func TestConfigureCottenDNSForPrivateListeners(t *testing.T) {
	spec, _ := FindSpec("cottendns")
	request := Request{Domain: "dns.example", PrivatePort: 5301, EnableTCP: true, EnableDoT: true, EnableDoH: true}
	plan := PortPlan{DoTPrivatePort: 8853, DoHPrivatePort: 8453, DoHMode: "behind-panel"}
	input := []byte("DOMAIN = [\"old\"]\nUDP_HOST = \"0.0.0.0\"\nUDP_PORT = 53\nDOH_TLS_ENABLED = true\n")
	got, err := configure(spec, request, plan, input)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"DOMAIN = [\"dns.example\"]", "UDP_HOST = \"127.0.0.1\"", "UDP_PORT = 5301", "TCP_IPV6_HOST = \"::1\"", "DOH_COEXIST_MODE = \"behind\"", "DOH_TLS_ENABLED = false", "DOH_BEHIND_PORT = 8453", "MAX_PACKET_SIZE = 16384", "TCP_MAX_CONNS_PER_IP = 32", "MAX_ACTIVE_SESSIONS = 1024"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestConfigureCottenDNSPreservesAdvancedCapacityTuning(t *testing.T) {
	spec, _ := FindSpec("cottendns")
	input := []byte("DOMAIN = [\"old\"]\nUDP_HOST = \"127.0.0.1\"\nUDP_PORT = 5301\nTCP_MAX_CONNS = 2048\nDOH_REQUESTS_PER_SECOND_PER_IP = 4096\nMAX_CONCURRENT_REQUESTS = 16384\nMAX_STREAMS_PER_SESSION = 4096\nMAX_PACKET_SIZE = 12000\nMAX_DNS_RESPONSE_BYTES = 12000\n")
	got, err := configure(spec, Request{Domain: "new.example", PrivatePort: 5301, EnableTCP: true}, PortPlan{DNSPort: 5301}, input)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"TCP_MAX_CONNS = 2048", "DOH_REQUESTS_PER_SECOND_PER_IP = 4096", "MAX_CONCURRENT_REQUESTS = 16384", "MAX_STREAMS_PER_SESSION = 4096", "MAX_PACKET_SIZE = 12000", "MAX_DNS_RESPONSE_BYTES = 12000"} {
		if !strings.Contains(text, want) {
			t.Fatalf("advanced value %q was overwritten:\n%s", want, text)
		}
	}
}

func TestCottenRouterFrontDoHRequiresUsableCertificatePath(t *testing.T) {
	spec, _ := FindSpec("cottendns")
	request := Request{Domain: "dns.example", DoHDomain: "doh.example", PrivatePort: 5301, EnableDoH: true}
	plan := PortPlan{DoHPublicPort: 443, DoHPrivatePort: 8443, DoHMode: "router-front"}
	if _, err := configure(spec, request, plan, []byte("DOMAIN = [\"old\"]\nACME_ENABLED = true\n")); err == nil || !strings.Contains(err.Error(), "private listener") {
		t.Fatalf("current upstream ACME limitation was not gated: %v", err)
	}
	manual := []byte("DOMAIN = [\"old\"]\nTLS_CERT_FILE = \"/etc/cert.pem\"\nTLS_KEY_FILE = \"/etc/key.pem\"\n")
	if _, err := configure(spec, request, plan, manual); err != nil {
		t.Fatalf("manual certificate was rejected: %v", err)
	}
	future := []byte("DOMAIN = [\"old\"]\nACME_EXTERNAL_PORT = 0\n")
	configured, err := configure(spec, request, plan, future)
	if err != nil || !strings.Contains(string(configured), "ACME_EXTERNAL_PORT = 443") {
		t.Fatalf("explicit upstream external-port support was not configured: %v\n%s", err, configured)
	}
}

func TestCottenRouterAlternateDoHPortRequiresManualCertificate(t *testing.T) {
	err := validateCottenRouterFrontDoHTLS([]byte("ACME_EXTERNAL_PORT = 443\n"), PortPlan{DoHPublicPort: 8443, DoHPrivatePort: 8553})
	if err == nil || !strings.Contains(err.Error(), "cannot use ACME") {
		t.Fatalf("alternate public port incorrectly accepted ACME: %v", err)
	}
}

func TestConfigureThefeedPreservesAdvancedSettings(t *testing.T) {
	spec, _ := FindSpec("thefeed")
	input := []byte("THEFEED_DOMAIN=old\nTHEFEED_KEY=keep-secret\nTHEFEED_LISTEN=0.0.0.0:53\n")
	got, err := configure(spec, Request{Domain: "feed.example", PrivatePort: 5304, ExtraDomains: "extra.example"}, PortPlan{}, input)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "THEFEED_KEY=keep-secret") || !strings.Contains(text, "THEFEED_LISTEN=127.0.0.1:5304") || !strings.Contains(text, "THEFEED_CHAT_MAX_ACCOUNTS=50000") {
		t.Fatalf("advanced settings were lost:\n%s", text)
	}
}

func TestConfigureThefeedPreservesExistingAccountLimit(t *testing.T) {
	spec, _ := FindSpec("thefeed")
	input := []byte("THEFEED_DOMAIN=old\nTHEFEED_LISTEN=127.0.0.1:5304\nTHEFEED_CHAT_MAX_ACCOUNTS=75000\n")
	got, err := configure(spec, Request{Domain: "feed.example", PrivatePort: 5304}, PortPlan{}, input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "THEFEED_CHAT_MAX_ACCOUNTS=75000") {
		t.Fatalf("existing account limit was overwritten:\n%s", got)
	}
}

func TestServiceNameValidation(t *testing.T) {
	for _, name := range []string{"cottendns", "slipgate-demo", "unit@instance"} {
		if !safeServiceName(name) {
			t.Fatalf("safe name rejected: %q", name)
		}
	}
	for _, name := range []string{"../escape", "bad/name", "bad name"} {
		if safeServiceName(name) {
			t.Fatalf("unsafe name accepted: %q", name)
		}
	}
}
