package tui

import (
	"fmt"
	"strings"

	"github.com/TaJirax/CottenRouter/internal/installer"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

func (m Model) terminalSize() (int, int) {
	w, h := m.width, m.height
	if w == 0 {
		w = 100
	}
	if h == 0 {
		h = 30
	}
	return w, h
}

func (m Model) bodyHeight() int {
	_, height := m.terminalSize()
	return max(5, height-6)
}

func fitLine(text string, width int) string { return ansi.Truncate(text, max(1, width), "…") }

func wrapped(text string, width int) []string {
	return strings.Split(ansi.Wrap(text, max(1, width), " "), "\n")
}

// Width is the whole frame, including its border and padding.
func frame(lines []string, width, maxHeight int) string {
	inside := max(1, width-4)
	if len(lines) > maxHeight-2 {
		lines = lines[:max(1, maxHeight-3)]
		lines = append(lines, "…")
	}
	result := []string{"╭" + strings.Repeat("─", max(1, width-2)) + "╮"}
	for _, line := range lines {
		line = fitLine(line, inside)
		result = append(result, "│ "+line+strings.Repeat(" ", max(0, inside-lipgloss.Width(line)))+" │")
	}
	result = append(result, "╰"+strings.Repeat("─", max(1, width-2))+"╯")
	return strings.Join(result, "\n")
}

func (m Model) View() string {
	width, height := m.terminalSize()
	if width < 50 || height < 20 {
		return fitLine("Resize to at least 50 columns × 20 rows. Ctrl+C quits.", width)
	}
	contentWidth := width - 4
	header := title.Render("COTTENROUTER CONTROL DECK")
	tabs := []string{"Overview", "Install", "Ports", "Guide"}
	for i := range tabs {
		if i == m.tab {
			tabs[i] = title.Render("[" + tabs[i] + "]")
		}
	}
	foot := "←/→ tabs · r refresh · q quit"
	body := ""
	if m.credentialView {
		body, foot = m.credentialsView(contentWidth), "↑/↓ scroll · Enter/Esc back · Ctrl+C quit"
	} else if m.form {
		body = m.formView(contentWidth)
		foot = "Tab/↑/↓ fields · Enter next/install · Esc cancel · Ctrl+C quit"
		if width < 75 {
			foot = "Ctrl+C quit · Tab fields · Enter next/install · Esc back"
		}
	} else {
		switch m.tab {
		case 0:
			body = m.overview(contentWidth)
		case 1:
			body = m.installView(contentWidth)
			foot = "Space select · i easy install · U update · Enter manage · a advanced\ns restart · u detach · x purge · v keys · V secrets · q quit"
			if width < 75 {
				foot = "Space select · i install · U update · q quit\nEnter manage · v keys · a advanced · u detach · x purge"
			}
		case 2:
			body = m.portsView(contentWidth)
		default:
			body = m.guideView(contentWidth)
		}
	}
	lines := []string{header, strings.Join(tabs, "  "), "", body}
	lines = append(lines, strings.Split(foot, "\n")...)
	rendered := strings.Join(lines, "\n")
	output := strings.Split(rendered, "\n")
	if len(output) > height {
		output = output[:height]
	}
	for i := range output {
		output[i] = "  " + fitLine(output[i], contentWidth)
	}
	return strings.Join(output, "\n")
}

func (m Model) overview(width int) string {
	inside := width - 4
	state := "ONLINE"
	if m.err != nil {
		state = "OFFLINE"
	}
	left := []string{title.Render(state), "Uptime " + duration(m.stats.UptimeSec),
		"In  " + humanBytes(totalIn(m.stats)), "Out " + humanBytes(totalOut(m.stats)),
		fmt.Sprintf("%s RAM · %d goroutines", humanBytes(m.stats.MemoryBytes), m.stats.Goroutines),
		fmt.Sprintf("Drops %d · limited %d", m.stats.Dropped, m.stats.Limited)}
	right := []string{title.Render("SERVICES")}
	for _, service := range m.services {
		right = append(right, fmt.Sprintf("%-13s %s", service.name, service.state))
	}
	rows := []string{}
	column := max(10, inside/2-1)
	for i := 0; i < max(len(left), len(right)); i++ {
		a, b := "", ""
		if i < len(left) {
			a = fitLine(left[i], column)
		}
		if i < len(right) {
			b = right[i]
		}
		rows = append(rows, a+strings.Repeat(" ", max(0, column-lipgloss.Width(a)))+"  "+b)
	}
	rows = append(rows, "", title.Render("ACTIVE PROTOCOLS"))
	available := max(1, m.bodyHeight()-2-len(rows))
	for i, metric := range m.stats.Protocols {
		if i >= available-1 && len(m.stats.Protocols) > available {
			rows = append(rows, fmt.Sprintf("+%d more protocol groups", len(m.stats.Protocols)-i))
			break
		}
		rows = append(rows, fmt.Sprintf("%-9s %-15s %s · sessions %d/%d", metric.Protocol, fitLine(metric.Route, 15), humanBytes(metric.BytesIn+metric.BytesOut), metric.Active, metric.Sessions))
	}
	if len(m.stats.Protocols) == 0 {
		rows = append(rows, "No traffic recorded yet. Refreshes every 2 seconds.")
	}
	return frame(rows, width, m.bodyHeight())
}

func (m Model) installView(width int) string {
	count := 0
	for _, selected := range m.selected {
		if selected {
			count++
		}
	}
	rows := []string{title.Render(fmt.Sprintf("PROJECT MANAGER · %d selected", count)), "    PROJECT          STATE        PORT"}
	maxRows := max(1, m.bodyHeight()-10)
	if m.projects[m.cursor].ID == "cottendns" && width < 71 {
		// Reserve room for the integrated Max beta requirements on narrow
		// terminals. The project list remains reachable with the arrow keys.
		maxRows = max(1, m.bodyHeight()-14)
	}
	start := max(0, m.cursor-maxRows+1)
	if len(m.projects) <= maxRows {
		start = 0
	}
	for i := start; i < min(len(m.projects), start+maxRows); i++ {
		spec := m.projects[i]
		cursor, check := " ", "[ ]"
		if i == m.cursor {
			cursor = "›"
		}
		if m.selected[i] {
			check = "[x]"
		}
		state := m.projectStates[spec.ID]
		status := "available"
		if state.Installed {
			status = "installed"
		}
		if state.Integrated {
			status = "integrated"
		}
		port := fmt.Sprintf(":%d", choosePort(state.PrivatePort, spec.DefaultPort))
		if spec.Kind == installer.ConfigSlipGate {
			port = "multi"
		}
		row := fmt.Sprintf("%s%s %-15s %-12s %s", cursor, check, spec.Name, status, port)
		if i == m.cursor {
			row = title.Render(row)
		}
		rows = append(rows, row)
	}
	if start > 0 || start+maxRows < len(m.projects) {
		rows = append(rows, fmt.Sprintf("↑/↓ more projects · %d/%d", m.cursor+1, len(m.projects)))
	}
	spec := m.projects[m.cursor]
	domain := m.projectStates[spec.ID].Domain
	if domain == "" {
		domain = "choose a delegated domain during installation"
	}
	rows = append(rows, "", fitLine(spec.Name+": "+domain, width-4))
	if spec.ID == "cottendns" {
		rows = append(rows, cottenMaxGuidance(width-4)...)
	}
	if version := m.projectStates[spec.ID].Version; version != "" {
		rows = append(rows, fitLine("Installed release: "+version, width-4))
	}
	rows = append(rows, wrapped("Soar: latest Lantern · Soar 2.2: WhiteDNS older Lantern v0.2.2 · amd64", width-4)...)
	if spec.Kind == installer.ConfigSlipGate {
		rows = append(rows, "SlipGate opens its native multi-tunnel wizard.")
	}
	if m.notice != "" {
		rows = append(rows, wrapped(m.notice, width-4)...)
	}
	if m.busy {
		rows = append(rows, "Installer is running in the terminal…")
	}
	return frame(rows, width, m.bodyHeight())
}

func (m Model) formView(width int) string {
	spec := m.projects[m.cursor]
	heading := strings.ToUpper(m.operation) + " " + strings.ToUpper(spec.Name)
	if m.batchTotal > 1 {
		heading += fmt.Sprintf(" · %d/%d", m.batchStep, m.batchTotal)
	}
	rows := []string{title.Render(heading), "Use your delegated domain. Private ports are planned automatically.", ""}
	labels := map[string]string{"domain": "Tunnel domain", "extra": "Extra domains", "port": "Private port", "capacity": "Capacity", "chat": "Chat domains", "dot": "DoT hostname", "doh": "DoH hostname"}
	for i, item := range m.fields {
		label := fmt.Sprintf("%-14s", labels[item.key])
		if i == m.fieldIndex {
			label = title.Render(label)
		}
		input := item.input
		input.Width = max(8, width-24)
		rows = append(rows, label+" "+input.View())
	}
	rows = append(rows, "")
	if spec.SupportsTCP {
		rows = append(rows, fmt.Sprintf("Ctrl+T TCP [%s] · Ctrl+D DoT [%s] · Ctrl+O DoH [%s]", onOff(m.enableTCP), onOff(m.enableDoT), onOff(m.enableDoH)))
		if m.enableDoH {
			rows = append(rows, "DoH needs native TLS certificate/key settings.")
		}
	}
	if spec.Kind == installer.ConfigSoar {
		origin := "Soar builds the latest official Lantern source."
		if spec.ID == "soar22" {
			origin = "WhiteDNS standalone bundle: older Lantern engine v0.2.2 (amd64)."
		}
		rows = append(rows, wrapped(origin, width-4)...)
		rows = append(rows, "One delegated zone · UDP-only · share its public key.")
	}
	if spec.ID == "cottendns" {
		rows = append(rows, cottenMaxGuidance(width-4)...)
	}
	if spec.ID == "thefeed" {
		rows = append(rows, "Next: native setup asks for sources and a passphrase.", "Add at least one public channel or X account.")
	}
	if spec.Kind == installer.ConfigTOML && spec.ID != "cottendns" {
		rows = append(rows, "UDP-only backend. Native setup keeps all project-specific options.")
	}
	if m.formError != "" {
		rows = append(rows, wrapped("Fix: "+m.formError, width-4)...)
	}
	return frame(rows, width, m.bodyHeight())
}

func (m Model) portsView(width int) string {
	rows := []string{title.Render("LISTENERS & PANEL PROTECTION"), "CottenRouter owns DNS/53; existing panels keep 443/853.", ""}
	seen := map[string]bool{}
	for _, item := range m.listeners {
		key := fmt.Sprintf("%s:%s:%d", item.Protocol, item.Address, item.Port)
		if seen[key] {
			continue
		}
		seen[key] = true
		rows = append(rows, fmt.Sprintf(":%-5d %-4s %-22s %s", item.Port, item.Protocol, item.Address, item.Process))
	}
	if len(rows) == 3 {
		rows = append(rows, "No listener data available. Press r to refresh.")
	}
	return frame(rows, width, m.bodyHeight())
}

func (m Model) guideView(width int) string {
	rows := []string{title.Render("EASY INSTALL"), "", "1. Install tab: Space selects; i starts the guided queue.", "2. Enter a unique delegated domain for each project.", "3. Tab moves fields; Enter on the last field installs.", "4. Native setup asks for project-specific options.", ""}
	rows = append(rows, cottenMaxGuidance(width-4)...)
	for _, line := range []string{
		"Latest published builds may not include Max yet.",
		"thefeed needs a source; SlipGate uses its native wizard.",
		"Soar and Soar 2.2 need different zones.",
		"Soar 2.2: WhiteDNS, older Lantern v0.2.2, amd64.",
		"DNS: suffix routing. DoT/DoH: TLS SNI.",
		"Existing panels keep ports; conflicts are reported.",
	} {
		rows = append(rows, wrapped(line, width-4)...)
	}
	return frame(rows, width, m.bodyHeight())
}

// Max is an internal CottenDNS mode, never a separately installable project.
// Avoid promising that an older published server supports the beta protocol.
func cottenMaxGuidance(width int) []string {
	rows := []string{}
	for _, line := range []string{
		"Normal + CottenMax beta share the domain/key.",
		"Max: compatible beta server; client selects mode.",
		"Capacity: keep existing, or opt in to expanded sessions.",
	} {
		rows = append(rows, wrapped(line, width)...)
	}
	return rows
}

func (m Model) credentialsView(width int) string {
	rows := []string{}
	for _, credential := range m.credentials {
		rows = append(rows, wrapped(credential.Label+": "+credential.Value, width-4)...)
		rows = append(rows, wrapped("Path: "+credential.Path, width-4)...)
		rows = append(rows, "")
	}
	available := max(1, m.bodyHeight()-4)
	start := min(m.credentialOffset, max(0, len(rows)-available))
	lines := []string{title.Render("KEYS · " + m.credentialProject), ""}
	lines = append(lines, rows[start:min(len(rows), start+available)]...)
	return frame(lines, width, m.bodyHeight())
}
