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
	for _, want := range []string{"DOMAIN = [\"dns.example\"]", "UDP_HOST = \"127.0.0.1\"", "UDP_PORT = 5301", "DOH_COEXIST_MODE = \"behind\"", "DOH_TLS_ENABLED = false", "DOH_BEHIND_PORT = 8453", "MAX_PACKET_SIZE = 16384", "TCP_MAX_CONNS_PER_IP = 32", "MAX_ACTIVE_SESSIONS = 1024"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
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
