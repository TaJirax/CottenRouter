package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/TaJirax/CottenRouter/internal/installer"
	"github.com/TaJirax/CottenRouter/internal/telemetry"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	cyan   = lipgloss.Color("#58D5E8")
	green  = lipgloss.Color("#63E6A5")
	yellow = lipgloss.Color("#FFD166")
	red    = lipgloss.Color("#FF6B7A")
	muted  = lipgloss.Color("#778899")
	panel  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#35475A")).Padding(1, 2)
	title  = lipgloss.NewStyle().Bold(true).Foreground(cyan)
	tabOn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#071018")).Background(cyan).Padding(0, 2)
	tabOff = lipgloss.NewStyle().Foreground(muted).Padding(0, 2)
)

type statusMsg struct {
	stats     telemetry.Snapshot
	services  []serviceState
	listeners []installer.Listener
	projects  []installer.ProjectState
	err       error
}
type serviceState struct{ name, state string }
type installDoneMsg struct {
	err                error
	operation, project string
}
type tickMsg time.Time

type field struct {
	key, label string
	input      textinput.Model
}

type Model struct {
	adminURL, routerConfig          string
	width, height, tab, cursor      int
	projects                        []installer.Spec
	projectStates                   map[string]installer.ProjectState
	selected                        map[int]bool
	stats                           telemetry.Snapshot
	services                        []serviceState
	listeners                       []installer.Listener
	err                             error
	form                            bool
	operation                       string
	queue                           []int
	confirm                         string
	fields                          []field
	fieldIndex                      int
	enableTCP, enableDoT, enableDoH bool
	notice                          string
}

func New(adminListen, routerConfig string) Model {
	if adminListen == "" {
		adminListen = "127.0.0.1:9088"
	}
	if routerConfig == "" {
		routerConfig = "/etc/cottenrouter/config.json"
	}
	return Model{adminURL: "http://" + adminListen + "/v1/status", routerConfig: routerConfig, projects: installer.Specs(), projectStates: map[string]installer.ProjectState{}, selected: map[int]bool{}, enableTCP: true}
}

func Run(adminListen, routerConfig string) error {
	_, err := tea.NewProgram(New(adminListen, routerConfig), tea.WithAltScreen()).Run()
	return err
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.refresh(), tick()) }

func tick() tea.Cmd {
	return tea.Tick(2*time.Second, func(value time.Time) tea.Msg { return tickMsg(value) })
}

func (m Model) refresh() tea.Cmd {
	previous := make(map[string]string, len(m.services))
	for _, service := range m.services {
		previous[service.name] = service.state
	}
	return func() tea.Msg {
		// Every probe gets its own budget. A single shared deadline made one slow
		// admin request or ss call expire the context for the systemctl probes
		// that follow, which then reported every service as offline.
		probe := func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 1500*time.Millisecond)
		}
		ctx, cancel := probe()
		defer cancel()
		msg := statusMsg{}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.adminURL, nil)
		if response, err := http.DefaultClient.Do(req); err == nil {
			defer response.Body.Close()
			if err := json.NewDecoder(response.Body).Decode(&msg.stats); err != nil {
				msg.err = err
			}
		} else {
			msg.err = err
		}
		serviceSpecs := []struct{ name, service string }{{"CottenRouter", "cottenrouter"}}
		for _, spec := range installer.Specs() {
			serviceSpecs = append(serviceSpecs, struct{ name, service string }{spec.Name, spec.Service})
		}
		for _, spec := range serviceSpecs {
			serviceCtx, serviceCancel := probe()
			// systemctl exits non-zero for inactive, failed, and unknown units but
			// still names the state on stdout. Trust the word, not the exit code.
			output, _ := exec.CommandContext(serviceCtx, "systemctl", "is-active", spec.service).Output()
			serviceCancel()
			state := strings.TrimSpace(string(output))
			if state == "" {
				// The probe itself failed. Keep the last known state rather than
				// claiming a running service went offline.
				if last, ok := previous[spec.name]; ok {
					state = last
				} else {
					state = "unknown"
				}
			}
			msg.services = append(msg.services, serviceState{name: spec.name, state: state})
		}
		listenerCtx, listenerCancel := probe()
		defer listenerCancel()
		if output, err := exec.CommandContext(listenerCtx, "ss", "-H", "-lntup").CombinedOutput(); err == nil {
			msg.listeners = installer.ParseSS(string(output))
		}
		msg.projects, _ = installer.Discover(m.routerConfig)
		return msg
	}
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case statusMsg:
		m.stats, m.services, m.listeners, m.err = msg.stats, msg.services, msg.listeners, msg.err
		for _, state := range msg.projects {
			m.projectStates[state.ID] = state
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case installDoneMsg:
		m.form = false
		operation := msg.operation
		if operation == "" {
			operation = m.operation
		}
		project := msg.project
		if project == "" && len(m.projects) > 0 && m.cursor >= 0 && m.cursor < len(m.projects) {
			project = m.projects[m.cursor].Name
		}
		if msg.err != nil {
			m.notice = fmt.Sprintf("%s for %s failed: %v", actionLabel(operation), project, msg.err)
			m.queue = nil
		} else {
			m.notice = fmt.Sprintf("%s for %s completed safely.", actionLabel(operation), project)
		}
		if msg.err == nil && operation == "install" && len(m.queue) > 0 {
			return m, m.startNextInstall()
		}
		m.operation = ""
		return m, m.refresh()
	case tea.KeyMsg:
		if m.form {
			return m.updateForm(msg)
		}
		if m.confirm != "" {
			if msg.String() == "esc" || msg.String() == "n" {
				m.confirm, m.notice = "", "Cancelled."
				return m, nil
			}
			if msg.String() == "y" {
				action := m.confirm
				m.confirm = ""
				m.operation = action
				if action == "reveal" {
					return m, m.keysCmd(true)
				}
				return m, m.removeCmd(action == "purge")
			}
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "q":
			return m, tea.Quit
		case "tab", "right", "l":
			m.tab = (m.tab + 1) % 4
		case "shift+tab", "left", "h":
			m.tab = (m.tab + 3) % 4
		case "up", "k":
			if m.tab == 1 && m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.tab == 1 && m.cursor < len(m.projects)-1 {
				m.cursor++
			}
		case " ":
			if m.tab == 1 {
				m.selected[m.cursor] = !m.selected[m.cursor]
			}
		case "enter", "e":
			if m.tab == 1 {
				spec := m.projects[m.cursor]
				state := m.projectStates[spec.ID]
				if spec.Kind == installer.ConfigSlipGate {
					m.queue = nil
					if state.Installed {
						m.operation = "advanced settings"
						return m, m.projectCmd("advanced")
					}
					m.operation = "install"
					return m, m.directInstallCmd()
				}
				operation := "install"
				if state.Installed {
					operation = "configure"
				}
				m.beginForm(operation)
			}
		case "i":
			if m.tab == 1 {
				return m, m.beginSelectedInstall()
			}
		case "u":
			if m.tab == 1 {
				state := m.projectStates[m.projects[m.cursor].ID]
				if !state.Installed {
					m.notice = "This project is not installed."
					return m, nil
				}
				if !state.Integrated {
					m.notice = "This project is already detached from CottenRouter."
					return m, nil
				}
				m.confirm, m.notice = "detach", "Detach this project and keep its files? Press y/n."
			}
		case "x":
			if m.tab == 1 {
				if !m.projectStates[m.projects[m.cursor].ID].Installed {
					m.notice = "This project is not installed."
					return m, nil
				}
				m.confirm, m.notice = "purge", "PERMANENTLY delete this managed project directory? Press y/n."
			}
		case "v":
			if m.tab == 1 {
				if !m.projectStates[m.projects[m.cursor].ID].Installed {
					m.notice = "Install this project before viewing its keys."
					return m, nil
				}
				m.operation = "keys"
				return m, m.keysCmd(false)
			}
		case "V":
			if m.tab == 1 {
				if !m.projectStates[m.projects[m.cursor].ID].Installed {
					m.notice = "Install this project before viewing its keys."
					return m, nil
				}
				m.confirm, m.notice = "reveal", "Reveal client secrets? Plaintext will remain in terminal scrollback. Press y/n."
			}
		case "a":
			if m.tab == 1 {
				if !m.projectStates[m.projects[m.cursor].ID].Installed {
					m.notice = "Install this project before opening Advanced settings."
					return m, nil
				}
				m.operation = "advanced settings"
				return m, m.projectCmd("advanced")
			}
		case "s":
			if m.tab == 1 {
				spec := m.projects[m.cursor]
				if !m.projectStates[spec.ID].Installed {
					m.notice = "Install this project before restarting its service."
					return m, nil
				}
				if spec.Kind == installer.ConfigSlipGate {
					m.notice = "Manage SlipGate's individual tunnel services through Advanced settings."
					return m, nil
				}
				m.operation = "service restart"
				return m, m.projectCmd("service", "--action", "restart")
			}
		case "r":
			return m, m.refresh()
		}
	}
	return m, nil
}

func (m Model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.form = false
		m.queue = nil
		m.operation = ""
		m.notice = "Installation cancelled."
		return m, nil
	case "ctrl+t":
		m.enableTCP = !m.enableTCP
		return m, nil
	case "ctrl+d":
		m.enableDoT = !m.enableDoT
		return m, nil
	case "ctrl+h":
		m.enableDoH = !m.enableDoH
		return m, nil
	case "up":
		if m.fieldIndex > 0 {
			m.fields[m.fieldIndex].input.Blur()
			m.fieldIndex--
			m.fields[m.fieldIndex].input.Focus()
		}
		return m, nil
	case "down", "enter":
		if m.fieldIndex < len(m.fields)-1 {
			m.fields[m.fieldIndex].input.Blur()
			m.fieldIndex++
			m.fields[m.fieldIndex].input.Focus()
			return m, nil
		}
		return m, m.installCmd()
	}
	var cmd tea.Cmd
	m.fields[m.fieldIndex].input, cmd = m.fields[m.fieldIndex].input.Update(msg)
	return m, cmd
}

func (m *Model) beginForm(operation string) {
	spec := m.projects[m.cursor]
	if spec.Kind == installer.ConfigSlipGate {
		m.form = false
		m.notice = "SlipGate uses its native multi-tunnel setup in Advanced settings."
		return
	}
	m.form, m.fieldIndex, m.notice, m.operation = true, 0, "", operation
	m.enableTCP, m.enableDoT, m.enableDoH = spec.SupportsTCP, false, false
	m.fields = nil
	state := m.projectStates[spec.ID]
	if state.Installed {
		m.enableTCP, m.enableDoT, m.enableDoH = state.EnableTCP, state.EnableDoT, state.EnableDoH
	}
	domain := spec.ID + ".example.com"
	port := spec.DefaultPort
	if state.Domain != "" {
		domain = state.Domain
	}
	if state.PrivatePort != 0 {
		port = state.PrivatePort
	}
	if spec.Kind != installer.ConfigSlipGate {
		m.fields = append(m.fields, newField("domain", "Tunnel domain", domain))
		m.fields = append(m.fields, newField("extra", "Additional DNS domains (comma separated)", state.ExtraDomains))
	}
	m.fields = append(m.fields, newField("port", "Private DNS port", strconv.Itoa(port)))
	if spec.ID == "thefeed" {
		m.fields = append(m.fields, newField("chat", "Chat domains (comma separated)", state.ChatDomains))
	}
	if spec.SupportsDoT {
		dotDomain, dohDomain := state.DoTDomain, state.DoHDomain
		if dotDomain == "" {
			dotDomain = "dot.example.com"
		}
		if dohDomain == "" {
			dohDomain = "doh.example.com"
		}
		m.fields = append(m.fields, newField("dot", "DoT hostname", dotDomain), newField("doh", "DoH hostname", dohDomain))
	}
	m.fields[0].input.Focus()
}

func (m *Model) beginSelectedInstall() tea.Cmd {
	m.queue = nil
	hadSelection := false
	for index := range m.projects {
		if m.selected[index] {
			hadSelection = true
		}
		if m.selected[index] && !m.projectStates[m.projects[index].ID].Installed {
			m.queue = append(m.queue, index)
		}
	}
	if hadSelection && len(m.queue) == 0 {
		m.notice = "Every selected project is already installed; use enter/e to edit it."
		return nil
	}
	if len(m.queue) == 0 {
		m.queue = []int{m.cursor}
	}
	return m.startNextInstall()
}

func (m *Model) startNextInstall() tea.Cmd {
	if len(m.queue) == 0 {
		return nil
	}
	m.cursor, m.queue = m.queue[0], m.queue[1:]
	m.operation = "install"
	if m.projects[m.cursor].Kind == installer.ConfigSlipGate {
		m.form = false
		return m.directInstallCmd()
	}
	m.beginForm("install")
	return nil
}

func newField(key, label, value string) field {
	input := textinput.New()
	input.SetValue(value)
	input.Prompt = "› "
	input.CharLimit = 250
	input.Width = 44
	return field{key: key, label: label, input: input}
}

func (m Model) installCmd() tea.Cmd {
	values := map[string]string{}
	for _, item := range m.fields {
		values[item.key] = item.input.Value()
	}
	spec := m.projects[m.cursor]
	executable, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return installDoneMsg{err: err, operation: m.operation, project: spec.Name} }
	}
	operation := m.operation
	if operation == "" {
		operation = "install"
	}
	args := []string{operation, "--project", spec.ID, "--domain", values["domain"], "--port", values["port"], "--router-config", m.routerConfig}
	if values["extra"] != "" {
		args = append(args, "--extra-domains", values["extra"])
	}
	if values["chat"] != "" {
		args = append(args, "--chat-domains", values["chat"])
	}
	if m.enableTCP {
		args = append(args, "--tcp")
	}
	if m.enableDoT {
		args = append(args, "--dot", "--dot-domain", values["dot"])
	}
	if m.enableDoH {
		args = append(args, "--doh", "--doh-domain", values["doh"])
	}
	command := exec.Command(executable, args...)
	return tea.ExecProcess(command, func(err error) tea.Msg {
		return installDoneMsg{err: err, operation: operation, project: spec.Name}
	})
}

func (m Model) directInstallCmd() tea.Cmd {
	spec := m.projects[m.cursor]
	executable, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return installDoneMsg{err: err, operation: "install", project: spec.Name} }
	}
	args := []string{"install", "--project", spec.ID, "--router-config", m.routerConfig}
	return tea.ExecProcess(exec.Command(executable, args...), func(err error) tea.Msg {
		return installDoneMsg{err: err, operation: "install", project: spec.Name}
	})
}

func (m Model) removeCmd(purge bool) tea.Cmd {
	executable, err := os.Executable()
	if err != nil {
		return func() tea.Msg {
			return installDoneMsg{err: err, operation: m.operation, project: m.projects[m.cursor].Name}
		}
	}
	spec := m.projects[m.cursor]
	args := []string{"remove", "--project", spec.ID, "--router-config", m.routerConfig}
	if purge {
		args = append(args, "--purge", "--confirm", spec.ID)
	}
	operation := "detach"
	if purge {
		operation = "purge"
	}
	return tea.ExecProcess(exec.Command(executable, args...), func(err error) tea.Msg {
		return installDoneMsg{err: err, operation: operation, project: spec.Name}
	})
}

func (m Model) keysCmd(reveal bool) tea.Cmd {
	executable, err := os.Executable()
	if err != nil {
		return func() tea.Msg {
			return installDoneMsg{err: err, operation: m.operation, project: m.projects[m.cursor].Name}
		}
	}
	args := []string{"keys", "--project", m.projects[m.cursor].ID}
	if reveal {
		args = append(args, "--show-secrets")
	}
	operation := "key lookup"
	if reveal {
		operation = "secret reveal"
	}
	project := m.projects[m.cursor].Name
	return tea.ExecProcess(exec.Command(executable, args...), func(err error) tea.Msg {
		return installDoneMsg{err: err, operation: operation, project: project}
	})
}

func (m Model) projectCmd(command string, extra ...string) tea.Cmd {
	executable, err := os.Executable()
	if err != nil {
		return func() tea.Msg {
			return installDoneMsg{err: err, operation: m.operation, project: m.projects[m.cursor].Name}
		}
	}
	args := []string{command, "--project", m.projects[m.cursor].ID}
	if command == "advanced" {
		args = append(args, "--router-config", m.routerConfig)
	}
	args = append(args, extra...)
	operation := m.operation
	if operation == "" {
		operation = command
	}
	project := m.projects[m.cursor].Name
	return tea.ExecProcess(exec.Command(executable, args...), func(err error) tea.Msg {
		return installDoneMsg{err: err, operation: operation, project: project}
	})
}

func (m Model) View() string {
	width := m.width
	if width < 80 {
		width = 80
	}
	header := title.Render("◉ COTTENROUTER CONTROL DECK") + "  " + lipgloss.NewStyle().Foreground(muted).Render("safe multi-protocol routing")
	tabs := []string{"Overview", "Install", "Ports", "Guide"}
	var rendered []string
	for i, item := range tabs {
		if i == m.tab {
			rendered = append(rendered, tabOn.Render(item))
		} else {
			rendered = append(rendered, tabOff.Render(item))
		}
	}
	body := ""
	if m.form {
		body = m.formView(width - 4)
	} else {
		switch m.tab {
		case 0:
			body = m.overview(width - 4)
		case 1:
			body = m.installView(width - 4)
		case 2:
			body = m.portsView(width - 4)
		default:
			body = m.guideView(width - 4)
		}
	}
	footer := lipgloss.NewStyle().Foreground(muted).Render("←/→ tabs  ↑/↓ move  space select  enter manage  r refresh  q quit")
	return lipgloss.NewStyle().Padding(1, 2).Render(header + "\n\n" + lipgloss.JoinHorizontal(lipgloss.Top, rendered...) + "\n\n" + body + "\n" + footer)
}

func (m Model) overview(width int) string {
	state, color := "ONLINE", green
	if m.err != nil {
		state, color = "OFFLINE", red
	}
	summary := fmt.Sprintf("%s\nUptime  %s\nTraffic %s in / %s out\nRouter  %s RAM · %d goroutines\nDrops   %d (%d limited)", lipgloss.NewStyle().Bold(true).Foreground(color).Render("● "+state), duration(m.stats.UptimeSec), humanBytes(totalIn(m.stats)), humanBytes(totalOut(m.stats)), humanBytes(m.stats.MemoryBytes), m.stats.Goroutines, m.stats.Dropped, m.stats.Limited)
	serviceLines := []string{title.Render("SERVICES")}
	for _, service := range m.services {
		mark := lipgloss.NewStyle().Foreground(red).Render("○")
		if service.state == "active" {
			mark = lipgloss.NewStyle().Foreground(green).Render("●")
		}
		serviceLines = append(serviceLines, fmt.Sprintf("%s %-16s %s", mark, service.name, service.state))
	}
	top := lipgloss.JoinHorizontal(lipgloss.Top, panel.Width(width/2-2).Render(summary), "  ", panel.Width(width/2-3).Render(strings.Join(serviceLines, "\n")))
	rows := []string{title.Render("ACTIVE PROTOCOLS")}
	max := uint64(1)
	for _, metric := range m.stats.Protocols {
		if metric.BytesIn+metric.BytesOut > max {
			max = metric.BytesIn + metric.BytesOut
		}
	}
	for _, metric := range m.stats.Protocols {
		amount := metric.BytesIn + metric.BytesOut
		rows = append(rows, fmt.Sprintf("%-12s %-18s %s %9s  sessions %d/%d", metric.Protocol, trim(metric.Route, 18), bar(amount, max, 18), humanBytes(amount), metric.Active, metric.Sessions))
	}
	if len(m.stats.Protocols) == 0 {
		rows = append(rows, lipgloss.NewStyle().Foreground(muted).Render("No traffic recorded yet. The display refreshes every 2 seconds."))
	}
	return top + "\n" + panel.Width(width).Render(strings.Join(rows, "\n")) + "\n"
}

func (m Model) installView(width int) string {
	lines := []string{title.Render("PROJECT MANAGER"), "Select several fresh projects for guided installation, or manage one installed project.", ""}
	for i, spec := range m.projects {
		cursor := "  "
		style := lipgloss.NewStyle()
		if i == m.cursor {
			cursor = "▸ "
			style = style.Bold(true).Foreground(cyan)
		}
		check := "[ ]"
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
		caps := "DNS"
		if spec.SupportsTCP {
			caps += " · TCP · DoT · DoH"
		}
		domain := state.Domain
		if domain == "" {
			domain = "-"
		}
		port := fmt.Sprintf(":%-5d", choosePort(state.PrivatePort, spec.DefaultPort))
		if spec.Kind == installer.ConfigSlipGate {
			port = "multi "
		}
		lines = append(lines, style.Render(fmt.Sprintf("%s%s %-15s %-10s %-6s %-25s %s", cursor, check, spec.Name, status, port, trim(domain, 25), caps)))
	}
	if m.notice != "" && m.confirm == "" {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(yellow).Render(m.notice))
	}
	if m.confirm != "" {
		lines = append(lines, "", lipgloss.NewStyle().Bold(true).Foreground(red).Render(m.notice))
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(muted).Render("space select · i install selected · enter/e setup or common settings · a advanced · s restart"))
	lines = append(lines, lipgloss.NewStyle().Foreground(muted).Render("u detach · x purge · v key paths · V reveal keys"))
	lines = append(lines, lipgloss.NewStyle().Foreground(muted).Render("Detach preserves data. Purge requires confirmation. SlipGate uses its native multi-tunnel setup."))
	return panel.Width(width).Render(strings.Join(lines, "\n")) + "\n"
}

func (m Model) formView(width int) string {
	spec := m.projects[m.cursor]
	lines := []string{title.Render(strings.ToUpper(m.operation) + " " + strings.ToUpper(spec.Name)), lipgloss.NewStyle().Foreground(muted).Render("The backend binds loopback only; CottenRouter owns public DNS ports."), ""}
	for i, item := range m.fields {
		label := lipgloss.NewStyle().Foreground(muted).Render(item.label)
		if i == m.fieldIndex {
			label = lipgloss.NewStyle().Bold(true).Foreground(cyan).Render(item.label)
		}
		lines = append(lines, label, item.input.View())
	}
	if spec.SupportsTCP {
		lines = append(lines, "", fmt.Sprintf("Ctrl+T TCP [%s]   Ctrl+D DoT [%s]   Ctrl+H DoH [%s]", onOff(m.enableTCP), onOff(m.enableDoT), onOff(m.enableDoH)))
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(yellow).Render("Enter on the last field starts the transactional installer · Esc cancels"))
	return panel.Width(width).Render(strings.Join(lines, "\n")) + "\n"
}

func (m Model) portsView(width int) string {
	lines := []string{title.Render("LISTENERS & PANEL PROTECTION"), "Public 53 is reserved for CottenRouter. Existing 443/853 owners are never stopped.", ""}
	for _, item := range m.listeners {
		if item.Port == 53 || item.Port == 443 || item.Port == 853 || (item.Port >= 5300 && item.Port < 9500) {
			lines = append(lines, fmt.Sprintf(":%-5d %-5s %-24s %s", item.Port, item.Protocol, trim(item.Address, 24), trim(item.Process, 45)))
		}
	}
	if len(lines) == 3 {
		lines = append(lines, lipgloss.NewStyle().Foreground(muted).Render("Listener scan unavailable or no relevant ports are active."))
	}
	return panel.Width(width).Render(strings.Join(lines, "\n")) + "\n"
}

func (m Model) guideView(width int) string {
	text := title.Render("HOW SHARING WORKS") + "\n\n" +
		"53/UDP + 53/TCP  CottenRouter selects a backend from the DNS suffix.\n" +
		"853/DoT          SNI routes encrypted streams to a private CottenDNS port.\n" +
		"443/DoH          If free, CottenRouter SNI-routes it. If a panel owns it,\n" +
		"                 CottenDNS uses behind-panel h2c and the panel remains in control.\n\n" +
		lipgloss.NewStyle().Foreground(yellow).Render("No installer kills an unknown listener. Conflicts stop the transaction with a report.")
	return panel.Width(width).Render(text) + "\n"
}

func totalIn(s telemetry.Snapshot) uint64 {
	var n uint64
	for _, item := range s.Protocols {
		n += item.BytesIn
	}
	return n
}
func totalOut(s telemetry.Snapshot) uint64 {
	var n uint64
	for _, item := range s.Protocols {
		n += item.BytesOut
	}
	return n
}
func humanBytes(value uint64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	number := float64(value)
	unit := 0
	for number >= 1024 && unit < len(units)-1 {
		number /= 1024
		unit++
	}
	return fmt.Sprintf("%.1f %s", number, units[unit])
}
func duration(seconds int64) string {
	return (time.Duration(seconds) * time.Second).Round(time.Second).String()
}
func bar(value, max uint64, size int) string {
	filled := int(float64(value) / float64(max) * float64(size))
	if filled > size {
		filled = size
	}
	return lipgloss.NewStyle().Foreground(cyan).Render(strings.Repeat("█", filled)) + lipgloss.NewStyle().Foreground(lipgloss.Color("#263645")).Render(strings.Repeat("░", size-filled))
}
func trim(value string, size int) string {
	if len(value) <= size {
		return value
	}
	return value[:size-1] + "…"
}
func onOff(value bool) string {
	if value {
		return "ON"
	}
	return "off"
}

func actionLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "Operation"
	}
	return strings.ToUpper(value[:1]) + value[1:]
}

func choosePort(current, fallback int) int {
	if current != 0 {
		return current
	}
	return fallback
}
