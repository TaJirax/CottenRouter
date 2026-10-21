package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	revision  int
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

type credentialsMsg struct {
	project string
	values  []installer.Credential
	err     error
}

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
	formError                       string
	busy                            bool
	revision                        int
	batchTotal, batchStep           int
	completed                       []string
	credentialView                  bool
	credentials                     []installer.Credential
	credentialProject               string
	credentialOffset                int
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

// slipGateTunnelState summarizes `systemctl list-units slipgate-*` output
// (UNIT LOAD ACTIVE SUB ...) across SlipGate's tunnel and proxy services.
// Its DNS router and firewall-restore units are not tunnels. An empty result
// means systemctl printed nothing, so the caller keeps the last known state.
func slipGateTunnelState(units string) string {
	active, failed, seen := false, false, false
	for line := range strings.SplitSeq(units, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[0] == "slipgate-dnsrouter.service" || fields[0] == "slipgate-iptables.service" {
			continue
		}
		seen = true
		active = active || fields[2] == "active"
		failed = failed || fields[2] == "failed"
	}
	switch {
	case active:
		return "active"
	case failed:
		return "failed"
	case seen:
		return "inactive"
	}
	return ""
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
		msg := statusMsg{revision: m.revision}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, m.adminURL, nil)
		if response, err := http.DefaultClient.Do(req); err == nil {
			defer response.Body.Close()
			if err := json.NewDecoder(response.Body).Decode(&msg.stats); err != nil {
				msg.err = err
			}
		} else {
			msg.err = err
		}
		type serviceSpec struct {
			name, service string
			slipGate      bool
		}
		serviceSpecs := []serviceSpec{{name: "CottenRouter", service: "cottenrouter"}}
		for _, spec := range installer.Specs() {
			serviceSpecs = append(serviceSpecs, serviceSpec{spec.Name, spec.Service, spec.Kind == installer.ConfigSlipGate})
		}
		for _, spec := range serviceSpecs {
			serviceCtx, serviceCancel := probe()
			var state string
			if spec.slipGate {
				// SlipGate's own DNS router is kept off on purpose (CottenRouter owns
				// :53), so its unit said "inactive" while every tunnel was running.
				output, err := exec.CommandContext(serviceCtx, "systemctl", "list-units", "--type=service", "--all", "--no-legend", "--plain", "slipgate-*.service").Output()
				if state = slipGateTunnelState(string(output)); state == "" && err == nil {
					state = "inactive" // SlipGate is not installed.
				}
			} else {
				// systemctl exits non-zero for inactive, failed, and unknown units but
				// still names the state on stdout. Trust the word, not the exit code.
				output, _ := exec.CommandContext(serviceCtx, "systemctl", "is-active", spec.service).Output()
				state = strings.TrimSpace(string(output))
			}
			serviceCancel()
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
		m.resizeInputs()
	case statusMsg:
		if msg.revision != m.revision || m.busy {
			return m, nil
		}
		m.stats, m.services, m.listeners, m.err = msg.stats, msg.services, msg.listeners, msg.err
		for _, state := range msg.projects {
			m.projectStates[state.ID] = state
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case credentialsMsg:
		if msg.err != nil {
			m.notice = "Key lookup failed: " + msg.err.Error()
			return m, nil
		}
		m.credentialView, m.credentials, m.credentialProject, m.credentialOffset = true, msg.values, msg.project, 0
	case installDoneMsg:
		m.busy = false
		m.revision++ // discard probes started before or during the child operation
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
			if (operation == "install" || operation == "configure") && len(m.fields) > 0 && m.projects[m.cursor].Kind != installer.ConfigSlipGate {
				m.form, m.formError = true, m.notice
				return m, m.refresh()
			}
		} else {
			m.notice = fmt.Sprintf("%s for %s completed safely.", actionLabel(operation), project)
			if operation == "install" {
				for index, spec := range m.projects {
					if spec.Name == project {
						delete(m.selected, index)
						state := m.projectStates[spec.ID]
						state.ID, state.Name, state.Installed, state.Integrated = spec.ID, spec.Name, true, true
						if spec.Kind != installer.ConfigSlipGate {
							if request, err := m.formRequest(); err == nil {
								state.Domain, state.ExtraDomains, state.ChatDomains, state.PrivatePort = request.Domain, request.ExtraDomains, request.ChatDomains, request.PrivatePort
							}
						}
						m.projectStates[spec.ID] = state
					}
				}
				m.completed = append(m.completed, project)
			}
		}
		if msg.err == nil && operation == "install" && len(m.queue) > 0 {
			command := m.startNextInstall()
			return m, tea.Batch(command, m.refresh())
		}
		if msg.err == nil && operation == "install" && len(m.completed) > 1 {
			m.notice = fmt.Sprintf("Installed %d projects: %s.", len(m.completed), strings.Join(m.completed, ", "))
		}
		m.operation = ""
		return m, m.refresh()
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		if m.credentialView {
			switch msg.String() {
			case "down", "j":
				m.credentialOffset++
			case "up", "k":
				m.credentialOffset = max(0, m.credentialOffset-1)
			case "pgdown":
				m.credentialOffset += 10
			case "pgup":
				m.credentialOffset = max(0, m.credentialOffset-10)
			}
			if msg.String() == "esc" || msg.String() == "q" || msg.String() == "enter" {
				m.credentialView, m.credentials, m.credentialProject = false, nil, ""
			}
			return m, nil
		}
		if m.busy {
			return m, nil
		}
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
				m.queue, m.completed, m.batchTotal, m.batchStep = nil, nil, 0, 0
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
		case "U":
			if m.tab == 1 {
				if !m.projectStates[m.projects[m.cursor].ID].Installed {
					m.notice = "Install this project before updating it."
					return m, nil
				}
				m.queue, m.completed = nil, nil
				m.operation = "update"
				return m, m.projectCmd("update")
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
	if m.form && len(m.fields) > 0 {
		var command tea.Cmd
		m.fields[m.fieldIndex].input, command = m.fields[m.fieldIndex].input.Update(message)
		return m, command
	}
	return m, nil
}

func (m Model) updateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.form = false
		m.queue = nil
		m.operation, m.formError = "", ""
		m.notice = "Installation cancelled."
		return m, nil
	case "ctrl+t":
		if m.projects[m.cursor].SupportsTCP {
			m.enableTCP = !m.enableTCP
		}
		return m, nil
	case "ctrl+d":
		if m.projects[m.cursor].SupportsDoT {
			m.enableDoT = !m.enableDoT
		}
		return m, nil
	case "ctrl+o":
		if m.projects[m.cursor].SupportsDoH {
			m.enableDoH = !m.enableDoH
		}
		return m, nil
	case "up", "shift+tab":
		if m.fieldIndex > 0 {
			m.fields[m.fieldIndex].input.Blur()
			m.fieldIndex--
			m.fields[m.fieldIndex].input.Focus()
		}
		return m, nil
	case "down", "tab", "enter":
		if m.fieldIndex < len(m.fields)-1 {
			m.fields[m.fieldIndex].input.Blur()
			m.fieldIndex++
			m.fields[m.fieldIndex].input.Focus()
			return m, nil
		}
		if msg.String() == "enter" {
			return m, m.installCmd()
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.formError = ""
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
	m.form, m.fieldIndex, m.notice, m.operation, m.formError = true, 0, "", operation, ""
	m.enableTCP, m.enableDoT, m.enableDoH = spec.SupportsTCP, false, false
	m.fields = nil
	state := m.projectStates[spec.ID]
	if state.Installed {
		m.enableTCP = spec.SupportsTCP && state.EnableTCP
		m.enableDoT = spec.SupportsDoT && state.EnableDoT
		m.enableDoH = spec.SupportsDoH && state.EnableDoH
	}
	domain := ""
	port := spec.DefaultPort
	if state.Domain != "" {
		domain = state.Domain
	}
	if state.PrivatePort != 0 {
		port = state.PrivatePort
	}
	if spec.Kind != installer.ConfigSlipGate {
		m.fields = append(m.fields, newField("domain", "Tunnel domain", domain))
		m.fields[len(m.fields)-1].input.Placeholder = spec.ID + ".example.com"
		if spec.Kind != installer.ConfigSoar {
			m.fields = append(m.fields, newField("extra", "Additional DNS domains (comma separated)", state.ExtraDomains))
		}
	}
	m.fields = append(m.fields, newField("port", "Private DNS port", strconv.Itoa(port)))
	if spec.ID == "cottendns" {
		m.fields = append(m.fields, newField("capacity", "Session capacity (keep / expanded)", "keep"))
	}
	if spec.ID == "thefeed" {
		m.fields = append(m.fields, newField("chat", "Chat domains (comma separated)", state.ChatDomains))
	}
	if spec.SupportsDoT {
		dotDomain, dohDomain := state.DoTDomain, state.DoHDomain
		m.fields = append(m.fields, newField("dot", "DoT hostname", dotDomain), newField("doh", "DoH hostname", dohDomain))
		m.fields[len(m.fields)-2].input.Placeholder = "dot.example.com (if enabled)"
		m.fields[len(m.fields)-1].input.Placeholder = "doh.example.com (if enabled)"
	}
	m.fields[0].input.Focus()
	m.resizeInputs()
}

func (m *Model) beginSelectedInstall() tea.Cmd {
	m.queue = nil
	m.completed, m.batchTotal, m.batchStep = nil, 0, 0
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
		if m.projectStates[m.projects[m.cursor].ID].Installed {
			if m.projects[m.cursor].Kind == installer.ConfigSlipGate {
				m.operation = "advanced settings"
				return m.projectCmd("advanced")
			}
			m.beginForm("configure")
			return nil
		}
		m.queue = []int{m.cursor}
	}
	m.batchTotal = len(m.queue)
	return m.startNextInstall()
}

func (m *Model) startNextInstall() tea.Cmd {
	if len(m.queue) == 0 {
		return nil
	}
	m.cursor, m.queue = m.queue[0], m.queue[1:]
	m.batchStep++
	m.operation = "install"
	if m.projects[m.cursor].Kind == installer.ConfigSlipGate {
		m.form = false
		m.fields = nil
		return m.directInstallCmd()
	}
	m.beginForm("install")
	return nil
}

func newField(key, label, value string) field {
	input := textinput.New()
	input.SetValue(value)
	input.Prompt = "› "
	input.CharLimit = 2048
	if key == "domain" || key == "dot" || key == "doh" {
		input.CharLimit = 253
	}
	if key == "port" {
		input.CharLimit = 5
	}
	input.Width = 44
	return field{key: key, label: label, input: input}
}

func (m Model) formRequest() (installer.Request, error) {
	values := map[string]string{}
	for _, item := range m.fields {
		values[item.key] = item.input.Value()
	}
	spec := m.projects[m.cursor]
	port, err := strconv.Atoi(strings.TrimSpace(values["port"]))
	if err != nil {
		return installer.Request{}, fmt.Errorf("private port must be a number between 1024 and 65535")
	}
	request := installer.Request{ProjectID: spec.ID, Domain: strings.TrimSpace(values["domain"]), PrivatePort: port,
		ExtraDomains: strings.TrimSpace(values["extra"]), ChatDomains: strings.TrimSpace(values["chat"]), RouterConfig: m.routerConfig,
		EnableTCP: m.enableTCP, EnableDoT: m.enableDoT, EnableDoH: m.enableDoH, DoTDomain: strings.TrimSpace(values["dot"]), DoHDomain: strings.TrimSpace(values["doh"])}
	if spec.ID == "cottendns" {
		switch strings.ToLower(strings.TrimSpace(values["capacity"])) {
		case "", "keep":
		case "expanded":
			request.ExpandedCapacity = true
		default:
			return installer.Request{}, fmt.Errorf("session capacity must be keep or expanded")
		}
	}
	_, err = request.Validate()
	if err == nil {
		zones := request.Domain + "," + request.ExtraDomains + "," + request.ChatDomains
		if request.EnableDoT {
			zones += "," + request.DoTDomain
		}
		if request.EnableDoH {
			zones += "," + request.DoHDomain
		}
		for _, zone := range strings.Split(zones, ",") {
			if zone == "" {
				continue
			}
			for _, label := range strings.Split(zone, ".") {
				for _, ch := range label {
					if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_') {
						return request, fmt.Errorf("domain %q must use DNS letters, numbers, hyphens or underscores (use punycode for international names)", zone)
					}
				}
			}
		}
	}
	if err == nil {
		for id, state := range m.projectStates {
			if id == spec.ID || !state.Integrated {
				continue
			}
			for _, own := range strings.Split(request.Domain+","+request.ExtraDomains+","+request.ChatDomains, ",") {
				for _, used := range strings.Split(state.Domain+","+state.ExtraDomains+","+state.ChatDomains, ",") {
					if own != "" && own == strings.ToLower(strings.TrimSuffix(strings.TrimSpace(used), ".")) {
						return request, fmt.Errorf("domain %s is already used by %s; choose a different delegated domain", own, id)
					}
				}
			}
		}
	}
	return request, err
}

func (m Model) installCommand() (*exec.Cmd, error) {
	request, err := m.formRequest()
	if err != nil {
		return nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	operation := m.operation
	if operation == "" {
		operation = "install"
	}
	args := []string{operation, "--project", request.ProjectID, "--domain", request.Domain, "--port", strconv.Itoa(request.PrivatePort), "--router-config", request.RouterConfig}
	if request.ExpandedCapacity {
		args = append(args, "--expanded-capacity")
	}
	if request.ExtraDomains != "" {
		args = append(args, "--extra-domains", request.ExtraDomains)
	}
	if request.ChatDomains != "" {
		args = append(args, "--chat-domains", request.ChatDomains)
	}
	if request.EnableTCP {
		args = append(args, "--tcp")
	}
	if request.EnableDoT {
		args = append(args, "--dot", "--dot-domain", request.DoTDomain)
	}
	if request.EnableDoH {
		args = append(args, "--doh", "--doh-domain", request.DoHDomain)
	}
	return exec.Command(executable, args...), nil
}

func (m *Model) installCmd() tea.Cmd {
	command, err := m.installCommand()
	if err != nil {
		m.formError = strings.ReplaceAll(err.Error(), "--domain", "Tunnel domain")
		key := "domain"
		if strings.Contains(m.formError, "port") {
			key = "port"
		}
		if strings.Contains(m.formError, "capacity") {
			key = "capacity"
		}
		if strings.Contains(m.formError, "DoT") {
			key = "dot"
		}
		if strings.Contains(m.formError, "DoH") {
			key = "doh"
		}
		for i := range m.fields {
			m.fields[i].input.Blur()
			if m.fields[i].key == key {
				m.fieldIndex = i
			}
		}
		m.fields[m.fieldIndex].input.Focus()
		return nil
	}
	m.busy, m.form = true, false
	m.revision++
	return runChild(command, m.operation, m.projects[m.cursor].Name)
}

func (m Model) directInstallCommand() (*exec.Cmd, error) {
	spec := m.projects[m.cursor]
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	args := []string{"install", "--project", spec.ID, "--router-config", m.routerConfig}
	return exec.Command(executable, args...), nil
}

func (m *Model) directInstallCmd() tea.Cmd {
	command, err := m.directInstallCommand()
	if err != nil {
		return func() tea.Msg {
			return installDoneMsg{err: err, operation: "install", project: m.projects[m.cursor].Name}
		}
	}
	m.busy = true
	m.revision++
	return runChild(command, "install", m.projects[m.cursor].Name)
}

// runChild hands the terminal to a cottenrouter subcommand. The TUI redraw
// wipes whatever it printed, so a failure used to surface only as "exit
// status 1"; keep its last stderr line (the real error) for the notice.
func runChild(command *exec.Cmd, operation, project string) tea.Cmd {
	tail := &lastLineWriter{}
	command.Stderr = io.MultiWriter(os.Stderr, tail)
	// A non-file Stderr makes Wait drain a pipe. Anything an installer leaves
	// running in the background would hold it open and freeze the TUI.
	command.WaitDelay = 2 * time.Second
	return tea.ExecProcess(command, func(err error) tea.Msg {
		if line := tail.String(); err != nil && line != "" {
			err = fmt.Errorf("%s (%w)", line, err)
		}
		return installDoneMsg{err: err, operation: operation, project: project}
	})
}

type lastLineWriter struct{ buf []byte }

func (w *lastLineWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > 4096 {
		w.buf = w.buf[len(w.buf)-4096:]
	}
	return len(p), nil
}

func (w *lastLineWriter) String() string {
	lines := strings.Split(strings.TrimSpace(string(w.buf)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func (m *Model) removeCmd(purge bool) tea.Cmd {
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
	m.busy = true
	m.revision++
	return runChild(exec.Command(executable, args...), operation, spec.Name)
}

func (m Model) keysCmd(reveal bool) tea.Cmd {
	spec := m.projects[m.cursor]
	return func() tea.Msg {
		values, err := installer.Credentials(spec.ID, reveal)
		return credentialsMsg{project: spec.Name, values: values, err: err}
	}
}

func (m *Model) projectCmd(command string, extra ...string) tea.Cmd {
	executable, err := os.Executable()
	if err != nil {
		return func() tea.Msg {
			return installDoneMsg{err: err, operation: m.operation, project: m.projects[m.cursor].Name}
		}
	}
	args := []string{command, "--project", m.projects[m.cursor].ID}
	if command == "advanced" || command == "update" {
		args = append(args, "--router-config", m.routerConfig)
	}
	args = append(args, extra...)
	operation := m.operation
	if operation == "" {
		operation = command
	}
	project := m.projects[m.cursor].Name
	m.busy = true
	m.revision++
	return runChild(exec.Command(executable, args...), operation, project)
}

func (m *Model) resizeInputs() {
	width := m.width
	if width == 0 {
		width = 100
	}
	for i := range m.fields {
		m.fields[i].input.Width = max(8, width-32)
	}
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
