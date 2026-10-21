package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TaJirax/CottenRouter/internal/installer"
	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
)

func fillDomain(m *Model, domain string) {
	for i := range m.fields {
		if m.fields[i].key == "domain" {
			m.fields[i].input.SetValue(domain)
		}
	}
}

func TestEasyInstallBuildsCorrectChildForEveryBackend(t *testing.T) {
	for index, spec := range installer.Specs() {
		t.Run(spec.ID, func(t *testing.T) {
			m := New("", "/tmp/custom-router.json")
			m.cursor = index
			if spec.Kind == installer.ConfigSlipGate {
				command, err := m.directInstallCommand()
				if err != nil || strings.Join(command.Args[1:], " ") != "install --project slipgate --router-config /tmp/custom-router.json" {
					t.Fatalf("wrong native wizard handoff: %v, %v", command, err)
				}
				return
			}
			m.beginForm("install")
			fillDomain(&m, spec.ID+".example")
			command, err := m.installCommand()
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Join(command.Args[1:], " ")
			for _, expected := range []string{"install --project " + spec.ID, "--domain " + spec.ID + ".example", fmt.Sprintf("--port %d", spec.DefaultPort), "--router-config /tmp/custom-router.json"} {
				if !strings.Contains(args, expected) {
					t.Fatalf("missing %q: %s", expected, args)
				}
			}
			if strings.Contains(args, "--tcp") != spec.SupportsTCP {
				t.Fatalf("unsupported TCP flags: %s", args)
			}
			if strings.Contains(args, "--dot") || strings.Contains(args, "--doh") {
				t.Fatalf("default enables unconfigured TLS: %s", args)
			}
		})
	}
}

func TestEasyInstallCompletesAllSelectionsInOrder(t *testing.T) {
	m := New("", "")
	m.tab = 1
	for i := range m.projects {
		m.selected[i] = true
	}
	m.beginSelectedInstall()
	for i, spec := range m.projects {
		if m.cursor != i || m.batchStep != i+1 || m.batchTotal != len(m.projects) {
			t.Fatalf("wrong queue step: cursor=%d step=%d total=%d", m.cursor, m.batchStep, m.batchTotal)
		}
		if spec.Kind != installer.ConfigSlipGate {
			fillDomain(&m, fmt.Sprintf("t%d.queue.example", i))
			if m.installCmd() == nil {
				t.Fatal(m.formError)
			}
		}
		updated, _ := m.Update(installDoneMsg{operation: "install", project: spec.Name})
		m = updated.(Model)
		if !m.projectStates[spec.ID].Installed || m.selected[i] {
			t.Fatalf("successful project was not recorded/deselected: %s", spec.ID)
		}
	}
	if m.form || m.busy || len(m.queue) != 0 || len(m.completed) != len(m.projects) || !strings.Contains(m.notice, fmt.Sprintf("Installed %d projects", len(m.projects))) {
		t.Fatalf("queue didn't finish: %+v", m)
	}
}

func TestInvalidGuidedInputRetainsQueueAndTypedValues(t *testing.T) {
	m := New("", "")
	m.selected[0], m.selected[5] = true, true
	m.beginSelectedInstall()
	fillDomain(&m, "invalid domain")
	if m.installCmd() != nil || !m.form || m.busy || len(m.queue) != 1 || m.fields[0].input.Value() != "invalid domain" || m.formError == "" {
		t.Fatalf("invalid entry escaped form: form=%v busy=%v queue=%v error=%q", m.form, m.busy, m.queue, m.formError)
	}
	fillDomain(&m, "valid.example")
	if m.installCmd() == nil {
		t.Fatal(m.formError)
	}
}

func TestFormNavigationDoesNotAccidentallyInstall(t *testing.T) {
	m := New("", "")
	m.beginForm("install")
	fillDomain(&m, "valid.example")
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyTab})
	m = updated.(Model)
	if m.fieldIndex != 1 {
		t.Fatal("Tab did not move focus")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = updated.(Model)
	if m.fieldIndex != 0 {
		t.Fatal("Shift+Tab did not move focus")
	}
	for range m.fields {
		updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
		m = updated.(Model)
	}
	updated, command := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = updated.(Model)
	if command != nil || m.busy || !m.form {
		t.Fatal("Down submitted the form")
	}
	updated, command = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if command == nil {
		t.Fatal("Ctrl+C was ignored in form")
	}
	if _, ok := command().(tea.QuitMsg); !ok {
		t.Fatal("Ctrl+C did not quit")
	}
}

func TestFormForwardsAsynchronousInputMessages(t *testing.T) {
	m := New("", "")
	m.beginForm("install")
	_, command := m.Update(cursor.Blink())
	if command == nil {
		t.Fatal("asynchronous text-input messages were discarded")
	}
}

func TestKeysStayVisibleAndSecretsAreClearedOnDismiss(t *testing.T) {
	m := New("", "")
	updated, _ := m.Update(credentialsMsg{project: "Soar 2.2", values: []installer.Credential{{Label: "Public key", Value: "visible-public-key"}}})
	m = updated.(Model)
	if !m.credentialView || !strings.Contains(m.View(), "visible-public-key") {
		t.Fatal("key result disappeared")
	}
	updated, _ = m.Update(tickMsg{})
	m = updated.(Model)
	if !m.credentialView {
		t.Fatal("refresh dismissed key view")
	}
	updated, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m = updated.(Model)
	if m.credentialView || len(m.credentials) != 0 || strings.Contains(m.View(), "visible-public-key") {
		t.Fatal("dismissed key data remained in model")
	}
}

func TestStaleRefreshDoesNotUndoSuccessfulInstall(t *testing.T) {
	m := New("", "")
	m.beginForm("install")
	fillDomain(&m, "valid.example")
	m.installCmd()
	updated, _ := m.Update(installDoneMsg{operation: "install", project: "CottenDNS"})
	m = updated.(Model)
	updated, _ = m.Update(statusMsg{revision: 0, projects: []installer.ProjectState{{ID: "cottendns"}}})
	m = updated.(Model)
	if !m.projectStates["cottendns"].Installed {
		t.Fatal("stale status undid successful installation")
	}
}
