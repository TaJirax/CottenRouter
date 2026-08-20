package tui

import (
	"strings"
	"testing"

	"github.com/TaJirax/CottenRouter/internal/telemetry"
	tea "github.com/charmbracelet/bubbletea"
)

func TestOverviewRendersOperationalData(t *testing.T) {
	model := New("127.0.0.1:9088", "/tmp/config.json")
	model.width = 110
	model.stats = telemetry.Snapshot{
		UptimeSec: 65, MemoryBytes: 2 << 20, Goroutines: 8,
		Protocols: []telemetry.Metric{{Protocol: "dns/udp", Route: "cottendns", BytesIn: 1024, BytesOut: 2048, Sessions: 4, Active: 1}},
	}
	model.services = []serviceState{{name: "CottenRouter", state: "active"}}
	view := model.View()
	for _, want := range []string{"COTTENROUTER CONTROL DECK", "ONLINE", "CottenRouter", "dns/udp", "cottendns", "2.0 MiB RAM"} {
		if !strings.Contains(view, want) {
			t.Fatalf("dashboard missing %q:\n%s", want, view)
		}
	}
}

func TestInstallFormKeepsNativeProjectChoices(t *testing.T) {
	model := New("", "")
	model.tab = 1
	model.cursor = 3 // thefeed
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if !got.form || len(got.fields) != 4 {
		t.Fatalf("thefeed form fields = %d, open=%v", len(got.fields), got.form)
	}
}
