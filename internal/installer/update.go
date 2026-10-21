package installer

import (
	"context"
	"fmt"
	"strings"
)

func updateRequest(state ProjectState, routerConfig string) (Request, error) {
	if !state.Installed {
		return Request{}, fmt.Errorf("%s is not installed; install it first", state.Name)
	}
	if !state.Integrated {
		return Request{}, fmt.Errorf("%s is detached; configure its integration before updating", state.Name)
	}
	request := Request{ProjectID: state.ID, Domain: state.Domain, ExtraDomains: state.ExtraDomains, ChatDomains: state.ChatDomains, PrivatePort: state.PrivatePort, EnableTCP: state.EnableTCP, EnableDoT: state.EnableDoT, EnableDoH: state.EnableDoH, DoTDomain: state.DoTDomain, DoHDomain: state.DoHDomain, RouterConfig: routerConfig}
	_, err := request.Validate()
	return request, err
}

// Update fetches the current installer and preserves the managed integration.
// It never upgrades or replaces the running CottenRouter executable.
func (m Manager) Update(ctx context.Context, projectID, routerConfig string, progress Progress) (PortPlan, error) {
	return m.UpdateWithPanelBetaBundle(ctx, projectID, routerConfig, "", progress)
}

func (m Manager) UpdateWithPanelBetaBundle(ctx context.Context, projectID, routerConfig, bundle string, progress Progress) (PortPlan, error) {
	if bundle != "" && projectID != "whitedns-panel" {
		return PortPlan{}, fmt.Errorf("--panel-beta-bundle is available only for WhiteDNS Panel")
	}
	if _, err := SpecFor(projectID); err != nil {
		return PortPlan{}, err
	}
	if routerConfig == "" {
		routerConfig = "/etc/cottenrouter/config.json"
	}
	states, err := Discover(routerConfig)
	if err != nil {
		return PortPlan{}, err
	}
	for _, state := range states {
		if state.ID != projectID {
			continue
		}
		request, err := updateRequest(state, routerConfig)
		if err != nil {
			return PortPlan{}, err
		}
		request.PanelBetaBundle = bundle
		return m.Install(ctx, request, progress)
	}
	return PortPlan{}, fmt.Errorf("unknown project %q", projectID)
}

func releaseVersionMatches(output, expected string) bool {
	for _, word := range strings.Fields(output) {
		if strings.Trim(word, "\"'(),;:") == expected {
			return true
		}
	}
	return false
}
