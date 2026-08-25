package installer

import (
	"strings"
	"testing"
)

func TestPlanPortsProtectsPanel(t *testing.T) {
	request := Request{ProjectID: "cottendns", PrivatePort: 5301, EnableDoT: true, EnableDoH: true}
	plan := PlanPorts([]Listener{{Port: 443, Process: "x-ui"}, {Port: 853, Process: "panel"}}, request)
	if plan.DoHMode != "router-front" || plan.DoHPublicPort == 443 || plan.DoTPublicPort == 853 || len(plan.Conflicts) != 2 {
		t.Fatalf("unsafe plan: %+v", plan)
	}
	if plan.DoHPublicPort == plan.DoHPrivatePort || plan.DoHPublicPort == plan.DoTPrivatePort {
		t.Fatalf("public DoH port collides with a private listener: %+v", plan)
	}
}

func TestRequestValidation(t *testing.T) {
	r := Request{ProjectID: "masterdnsvpn", Domain: "VPN.Example.", ExtraDomains: " Extra.Example.,extra.example ", EnableDoH: true}
	if _, err := r.Validate(); err == nil {
		t.Fatal("expected unsupported DoH error")
	}
	r.EnableDoH = false
	if _, err := r.Validate(); err != nil || r.Domain != "vpn.example" || r.ExtraDomains != "extra.example" || r.PrivatePort != 5302 {
		t.Fatalf("validation failed: %v %+v", err, r)
	}
}

func TestPlanPortsNeverCollidesPublicAndPrivateDoT(t *testing.T) {
	request := Request{ProjectID: "cottendns", PrivatePort: 5301, EnableDoT: true}
	plan := PlanPorts([]Listener{{Port: 853, Process: "panel"}}, request)
	if plan.DoTPublicPort == plan.DoTPrivatePort {
		t.Fatalf("DoT ports collide: %+v", plan)
	}
}

func TestPlanPortsReservesEncryptedPrivatePorts(t *testing.T) {
	request := Request{ProjectID: "cottendns", PrivatePort: 5301, EnableDoT: true, EnableDoH: true}
	occupied := []Listener{{Port: 443, Process: "panel"}}
	for port := 8453; port < 8853; port++ {
		occupied = append(occupied, Listener{Port: port, Process: "occupied"})
	}
	plan := PlanPorts(occupied, request)
	if plan.DoTPrivatePort == 0 || plan.DoHPrivatePort == 0 {
		t.Fatalf("expected private encrypted ports: %+v", plan)
	}
	if plan.DoTPrivatePort == plan.DoHPrivatePort {
		t.Fatalf("DoT and DoH private ports collide: %+v", plan)
	}
	if plan.DoTPublicPort == plan.DoTPrivatePort || plan.DoTPublicPort == plan.DoHPrivatePort {
		t.Fatalf("public DoT port collides with a private listener: %+v", plan)
	}
	if plan.DoHPublicPort == plan.DoHPrivatePort || plan.DoHPublicPort == plan.DoTPrivatePort {
		t.Fatalf("public DoH port collides with a private listener: %+v", plan)
	}
}

func TestThefeedUsesCurrentUpstreamConfigPath(t *testing.T) {
	spec, ok := FindSpec("thefeed")
	if !ok {
		t.Fatal("thefeed spec missing")
	}
	if spec.ConfigPath != "/opt/thefeed/data/thefeed.env" {
		t.Fatalf("thefeed config path = %q", spec.ConfigPath)
	}
}

func TestRequiredBackendListenerMustBeOwnedAndLoopback(t *testing.T) {
	spec, _ := FindSpec("cottendns")
	expected := []expectedPrivateListener{{protocol: "udp", port: 5301}, {protocol: "tcp", port: 5301}}
	listeners := []Listener{
		{Protocol: "udp", Port: 5301, Address: "127.0.0.1:5301", Process: `users:(("cottendns",pid=10))`},
		{Protocol: "tcp", Port: 5301, Address: "0.0.0.0:5301", Process: `users:(("cottendns",pid=10))`},
	}
	missing := missingPrivateListeners(listeners, spec, expected)
	if len(missing) != 1 || missing[0].protocol != "tcp" {
		t.Fatalf("public TCP listener was accepted: %+v", missing)
	}
	listeners[1].Address = "[::1]:5301"
	if missing := missingPrivateListeners(listeners, spec, expected); len(missing) != 0 {
		t.Fatalf("valid loopback listeners were rejected: %+v", missing)
	}
}

// `cottenrouter keys` with no flag used to fail with `unknown project ""`,
// which reads like a broken install rather than a missing argument.
func TestSpecForNamesTheFlagAndTheValidProjects(t *testing.T) {
	if _, err := SpecFor(""); err == nil {
		t.Fatal("empty project ID was accepted")
	} else if !strings.Contains(err.Error(), "--project is required") || !strings.Contains(err.Error(), "cottendns") {
		t.Fatalf("empty project error = %q", err)
	}
	if _, err := SpecFor("bogus"); err == nil {
		t.Fatal("unknown project ID was accepted")
	} else if !strings.Contains(err.Error(), `unknown project "bogus"`) || !strings.Contains(err.Error(), "slipgate") {
		t.Fatalf("unknown project error = %q", err)
	}
	spec, err := SpecFor("cottendns")
	if err != nil || spec.ID != "cottendns" {
		t.Fatalf("SpecFor(cottendns) = %v %v", spec.ID, err)
	}
}

func TestRequestValidateNamesMissingDomainFlag(t *testing.T) {
	request := Request{ProjectID: "cottendns"}
	if _, err := request.Validate(); err == nil {
		t.Fatal("missing domain was accepted")
	} else if !strings.Contains(err.Error(), "--domain is required") {
		t.Fatalf("missing domain error = %q", err)
	}
	// SlipGate imports its domains from its own config and takes no --domain.
	if _, err := (&Request{ProjectID: "slipgate"}).Validate(); err != nil {
		t.Fatalf("SlipGate request rejected: %v", err)
	}
}
