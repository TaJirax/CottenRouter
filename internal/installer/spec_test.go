package installer

import "testing"

func TestPlanPortsProtectsPanel(t *testing.T) {
	request := Request{ProjectID: "cottendns", PrivatePort: 5301, EnableDoT: true, EnableDoH: true}
	plan := PlanPorts([]Listener{{Port: 443, Process: "x-ui"}, {Port: 853, Process: "panel"}}, request)
	if plan.DoHMode != "behind-panel" || plan.DoHPublicPort != 443 || plan.DoTPublicPort == 853 || len(plan.Conflicts) != 2 {
		t.Fatalf("unsafe plan: %+v", plan)
	}
}

func TestRequestValidation(t *testing.T) {
	r := Request{ProjectID: "masterdnsvpn", Domain: "VPN.Example.", EnableDoH: true}
	if _, err := r.Validate(); err == nil {
		t.Fatal("expected unsupported DoH error")
	}
	r.EnableDoH = false
	if _, err := r.Validate(); err != nil || r.Domain != "vpn.example" || r.PrivatePort != 5302 {
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
