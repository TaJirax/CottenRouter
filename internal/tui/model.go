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
	err       error
}
type serviceState struct{ name, state string }
type installDoneMsg struct{ err error }
type tickMsg time.Time

type field struct {
	key, label string
	input      textinput.Model
}

type Model struct {
	adminURL, routerConfig          string
	width, height, tab, cursor      int
	projects                        []installer.Spec
	stats                           telemetry.Snapshot
	services                        []serviceState
	listeners                       []installer.Listener
	err                             error
	form                            bool
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
	return Model{adminURL: "http://" + adminListen + "/v1/status", routerConfig: routerConfig, projects: installer.Specs(), enableTCP: true}
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
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
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
			state := "not installed"
			if output, err := exec.CommandContext(ctx, "systemctl", "is-active", spec.service).Output(); err == nil {
				state = strings.TrimSpace(string(output))
			}
			msg.services = append(msg.services, serviceState{name: spec.name, state: state})
		}
		if output, err := exec.CommandContext(ctx, "ss", "-H", "-lntup").CombinedOutput(); err == nil {
			msg.listeners = installer.ParseSS(string(output))
		}
		return msg
	}
}

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case statusMsg:
		m.stats, m.services, m.listeners, m.err = msg.stats, msg.services, msg.listeners, msg.err
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case installDoneMsg:
		m.form = false
		if msg.err != nil {
			m.notice = "Installation failed: " + msg.err.Error()
		} else {
			m.notice = "Installation completed and CottenRouter was restored."
		}
		return m, m.refresh()
	case tea.KeyMsg:
		if m.form {
			return m.updateForm(msg)
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
		case "enter":
			if m.tab == 1 {
				m.beginForm()
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

func (m *Model) beginForm() {
	spec := m.projects[m.cursor]
	m.form, m.fieldIndex, m.notice = true, 0, ""
	m.enableTCP, m.enableDoT, m.enableDoH = spec.SupportsTCP, false, false
	m.fields = nil
	if spec.Kind != installer.ConfigSlipGate {
		m.fields = append(m.fields, newField("domain", "Tunnel domain", spec.ID+".example.com"))
	}
	m.fields = append(m.fields, newField("port", "Private DNS port", strconv.Itoa(spec.DefaultPort)))
	if spec.ID == "thefeed" {
		m.fields = append(m.fields, newField("extra", "Extra domains (comma separated)", ""), newField("chat", "Chat domains (comma separated)", ""))
	}
	if spec.SupportsDoT {
		m.fields = append(m.fields, newField("dot", "DoT hostname", "dot.example.com"), newField("doh", "DoH hostname", "doh.example.com"))
	}
	m.fields[0].input.Focus()
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
		return func() tea.Msg { return installDoneMsg{err: err} }
	}
	args := []string{"install", "--project", spec.ID, "--domain", values["domain"], "--port", values["port"], "--router-config", m.routerConfig}
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
	return tea.ExecProcess(command, func(err error) tea.Msg { return installDoneMsg{err: err} })
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
	footer := lipgloss.NewStyle().Foreground(muted).Render("←/→ tabs  ↑/↓ select  enter open  r refresh  q quit")
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
	lines := []string{title.Render("INSTALL & INTEGRATE"), "Verified-current installers run one at a time with rollback and protected ports.", ""}
	for i, spec := range m.projects {
		cursor := "  "
		style := lipgloss.NewStyle()
		if i == m.cursor {
			cursor = "▸ "
			style = style.Bold(true).Foreground(cyan)
		}
		caps := "DNS"
		if spec.SupportsTCP {
			caps += " · TCP · DoT · DoH"
		}
		lines = append(lines, style.Render(fmt.Sprintf("%s%-16s  private :%-5d  %s", cursor, spec.Name, spec.DefaultPort, caps)))
	}
	if m.notice != "" {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(yellow).Render(m.notice))
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(muted).Render("Enter configures the selected project. Upstream advanced prompts remain available during installation."))
	return panel.Width(width).Render(strings.Join(lines, "\n")) + "\n"
}

func (m Model) formView(width int) string {
	spec := m.projects[m.cursor]
	lines := []string{title.Render("CONFIGURE " + strings.ToUpper(spec.Name)), lipgloss.NewStyle().Foreground(muted).Render("The backend binds loopback only; CottenRouter owns public DNS ports."), ""}
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
