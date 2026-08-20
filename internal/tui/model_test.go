package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/TaJirax/CottenRouter/internal/installer"
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

func TestProjectManagerSelectionAndInstalledState(t *testing.T) {
	model := New("", "")
	model.width, model.tab = 120, 1
	model.projectStates["cottendns"] = installer.ProjectState{ID: "cottendns", Installed: true, Integrated: true, Domain: "dns.example", PrivatePort: 5301}
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{' '}})
	got := updated.(Model)
	view := got.View()
	for _, want := range []string{"[x]", "integrated", "dns.example", "advanced", "detach", "purge"} {
		if !strings.Contains(view, want) {
			t.Fatalf("project manager missing %q:\n%s", want, view)
		}
	}
}

func TestCancellingFormClearsQueuedInstalls(t *testing.T) {
	model := New("", "")
	model.tab = 1
	model.cursor = 0
	model.queue = []int{1, 2}
	model.beginForm("install")
	updated, _ := model.Update(tea.KeyMsg{Type: tea.KeyEsc})
	got := updated.(Model)
	if got.form || len(got.queue) != 0 {
		t.Fatalf("cancel left form=%v queue=%v", got.form, got.queue)
	}
	if !strings.Contains(strings.ToLower(got.notice), "cancelled") {
		t.Fatalf("cancel notice = %q", got.notice)
	}
}

func TestFailedInstallClearsQueueAndNamesProject(t *testing.T) {
	model := New("", "")
	model.queue = []int{1, 2}
	updated, _ := model.Update(installDoneMsg{
		err:       errors.New("installer exited"),
		operation: "install",
		project:   "CottenDNS",
	})
	got := updated.(Model)
	if len(got.queue) != 0 {
		t.Fatalf("failed install left queue=%v", got.queue)
	}
	for _, want := range []string{"Install", "CottenDNS", "installer exited"} {
		if !strings.Contains(got.notice, want) {
			t.Fatalf("contextual notice missing %q: %q", want, got.notice)
		}
	}
}

func TestUninstalledProjectActionsAreGated(t *testing.T) {
	for _, key := range []rune{'u', 'x', 'v', 'V', 'a', 's'} {
		model := New("", "")
		model.tab = 1
		updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{key}})
		got := updated.(Model)
		if cmd != nil || got.confirm != "" {
			t.Fatalf("key %q launched unavailable action: cmd=%v confirm=%q", key, cmd != nil, got.confirm)
		}
		if got.notice == "" {
			t.Fatalf("key %q did not explain why it was unavailable", key)
		}
	}
}

func TestInstalledSlipGateUsesNativeAdvancedSettings(t *testing.T) {
	model := New("", "")
	model.tab = 1
	model.cursor = 4
	model.projectStates["slipgate"] = installer.ProjectState{ID: "slipgate", Installed: true, Integrated: true}
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.form || cmd == nil || got.operation != "advanced settings" {
		t.Fatalf("SlipGate did not use Advanced: form=%v cmd=%v operation=%q", got.form, cmd != nil, got.operation)
	}

	model = got
	updated, cmd = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'s'}})
	got = updated.(Model)
	if cmd != nil || !strings.Contains(got.notice, "individual tunnel") {
		t.Fatalf("SlipGate exposed a fake aggregate restart: cmd=%v notice=%q", cmd != nil, got.notice)
	}
}

func TestFreshSlipGateStartsInstallerWithoutSinglePortForm(t *testing.T) {
	model := New("", "")
	model.tab = 1
	model.cursor = 4
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	got := updated.(Model)
	if got.form || cmd == nil || got.operation != "install" {
		t.Fatalf("fresh SlipGate opened misleading form: form=%v cmd=%v operation=%q", got.form, cmd != nil, got.operation)
	}
	got.width = 120
	view := got.View()
	if strings.Contains(view, ":5310") || !strings.Contains(view, "multi") {
		t.Fatalf("SlipGate row still presents a single port:\n%s", view)
	}
}

func TestSecretRevealWarnsAboutTerminalScrollback(t *testing.T) {
	model := New("", "")
	model.tab = 1
	model.projectStates["cottendns"] = installer.ProjectState{ID: "cottendns", Installed: true}
	updated, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'V'}})
	got := updated.(Model)
	if cmd != nil || got.confirm != "reveal" || !strings.Contains(strings.ToLower(got.notice), "scrollback") {
		t.Fatalf("secret warning is incomplete: cmd=%v confirm=%q notice=%q", cmd != nil, got.confirm, got.notice)
	}
}
