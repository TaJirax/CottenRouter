package installer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ssRunner answers the listener scan with a fixture and records every command.
type ssRunner struct {
	ss       string
	unitList string
	runs     []string
}

func (runner *ssRunner) Run(_ context.Context, name string, args []string, _ string, _ bool) error {
	runner.runs = append(runner.runs, name+" "+strings.Join(args, " "))
	return nil
}

func (runner *ssRunner) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	if name == "ss" {
		return []byte(runner.ss), nil
	}
	if name == "systemctl" && len(args) > 0 && args[0] == "list-unit-files" {
		return []byte(runner.unitList), nil
	}
	return nil, nil
}

func routerConfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	body := `{"listen_udp":"0.0.0.0:53","listen_tcp":"0.0.0.0:53","admin_listen":"127.0.0.1:9088",` +
		`"routes":[{"name":"cottendns","domains":["dns.example"],"backend":"127.0.0.1:5301"}]}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every upstream backend installer takes port 53 for itself. The backend must
// be restarted onto its loopback port before the router reclaims 53, and a
// backend that never let go has to be named rather than surfacing as a bare
// systemd failure.
func TestRouterWaitsForPortHandbackAndNamesWhoeverKeepsIt(t *testing.T) {
	stuck := &ssRunner{ss: `udp UNCONN 0 0 0.0.0.0:53 0.0.0.0:* users:(("cottendns",pid=42,fd=3))`}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	err := Manager{Runner: stuck}.waitForRouterPortsReleased(ctx, routerConfigFile(t))
	if err == nil {
		t.Fatal("a backend still holding port 53 was not reported")
	}
	if !strings.Contains(err.Error(), "53/udp") || !strings.Contains(err.Error(), "cottendns") {
		t.Fatalf("error does not name the port and its owner: %v", err)
	}

	// The backend moved to loopback: only CottenRouter's own sockets remain,
	// and its admin port must not be mistaken for a squatter either.
	free := &ssRunner{ss: strings.Join([]string{
		`udp UNCONN 0 0 127.0.0.1:5301 0.0.0.0:* users:(("cottendns",pid=42,fd=3))`,
		`tcp LISTEN 0 4096 127.0.0.1:9088 0.0.0.0:* users:(("cottenrouter",pid=7,fd=9))`,
	}, "\n")}
	if err := (Manager{Runner: free}).waitForRouterPortsReleased(context.Background(), routerConfigFile(t)); err != nil {
		t.Fatalf("released ports were still treated as blocked: %v", err)
	}
}

// `enable --now` does nothing to a unit the upstream installer already started,
// which left the backend on port 53 with its pre-CottenRouter config.
func TestManagedSlipGateServicesAreRestartedOntoTheirPrivatePorts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "slipgate.json")
	body := `{"tunnels":[{"tag":"dns","enabled":true,"transport":"dns"}]}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	runner := &ssRunner{unitList: "slipgate-dns.service enabled\n"}
	if err := (Manager{Runner: runner}).enableSlipGateManagedServices(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	commands := strings.Join(runner.runs, "\n")
	if strings.Contains(commands, "enable --now") {
		t.Fatalf("an already-running unit was never restarted onto its new config:\n%s", commands)
	}
	for _, want := range []string{"systemctl enable slipgate-dns", "systemctl restart slipgate-dns"} {
		if !strings.Contains(commands, want) {
			t.Fatalf("missing %q in:\n%s", want, commands)
		}
	}
}
