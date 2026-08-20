package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type CommandStyle string

const (
	BashProcessSubstitution CommandStyle = "bash-process-substitution"
	PipeToSudoBash          CommandStyle = "pipe-to-sudo-bash"
)

type Project struct {
	ID             string
	Name           string
	RepoFullName   string
	Repository     string
	InstallerPath  string
	InstallerURL   string
	InstallCommand string
	DefaultBranch  string
	Service        string
	ListenSetting  string
	DefaultBackend string
	Routable       bool
	CommandStyle   CommandStyle
}

type Resolver struct {
	Client  *http.Client
	APIBase string
	RawBase string
}

func Projects() []Project {
	projects := []Project{
		{ID: "cottendns", Name: "CottenDNS", RepoFullName: "TaJirax/CottenDns", InstallerPath: "server_linux_install.sh", CommandStyle: BashProcessSubstitution, Service: "cottendns", ListenSetting: "UDP_HOST=127.0.0.1, UDP_PORT=5301; TCP_LISTENER_ENABLED=true", DefaultBackend: "127.0.0.1:5301", Routable: true},
		{ID: "masterdnsvpn", Name: "MasterDnsVPN", RepoFullName: "masterking32/MasterDnsVPN", InstallerPath: "server_linux_install.sh", CommandStyle: BashProcessSubstitution, Service: "masterdnsvpn", ListenSetting: "UDP_HOST=127.0.0.1, UDP_PORT=5302", DefaultBackend: "127.0.0.1:5302", Routable: true},
		{ID: "stormdns", Name: "StormDNS", RepoFullName: "nullroute1970/StormDNS", InstallerPath: "server_linux_install.sh", CommandStyle: BashProcessSubstitution, Service: "stormdns", ListenSetting: "UDP_HOST=127.0.0.1, UDP_PORT=5303", DefaultBackend: "127.0.0.1:5303", Routable: true},
		{ID: "thefeed", Name: "thefeed", RepoFullName: "sartoopjj/thefeed", InstallerPath: "scripts/install.sh", CommandStyle: PipeToSudoBash, Service: "thefeed-server", ListenSetting: "THEFEED_LISTEN=127.0.0.1:5304", DefaultBackend: "127.0.0.1:5304", Routable: true},
		{ID: "slipgate", Name: "SlipGate", RepoFullName: "anonvector/slipgate", InstallerPath: "install.sh", CommandStyle: PipeToSudoBash, Service: "slipgate-dnsrouter", ListenSetting: "Import /etc/slipgate/config.json; disable slipgate-dnsrouter", Routable: false},
	}
	for i := range projects {
		applyBranch(&projects[i], "main", "https://raw.githubusercontent.com")
	}
	return projects
}

func DefaultResolver() Resolver {
	return Resolver{Client: &http.Client{Timeout: 20 * time.Second}, APIBase: "https://api.github.com", RawBase: "https://raw.githubusercontent.com"}
}

// Latest resolves every repository's current default branch through GitHub's
// API and verifies that the installer exists on that branch. Installers never
// rely on a release number cached in CottenRouter.
func (r Resolver) Latest(ctx context.Context) ([]Project, error) {
	if r.Client == nil {
		r.Client = &http.Client{Timeout: 20 * time.Second}
	}
	if r.APIBase == "" {
		r.APIBase = "https://api.github.com"
	}
	if r.RawBase == "" {
		r.RawBase = "https://raw.githubusercontent.com"
	}
	projects := Projects()
	for i := range projects {
		branch, err := r.defaultBranch(ctx, projects[i].RepoFullName)
		if err != nil {
			return nil, err
		}
		applyBranch(&projects[i], branch, r.RawBase)
		if err := r.verifyInstaller(ctx, projects[i]); err != nil {
			return nil, err
		}
	}
	return projects, nil
}

func (r Resolver) defaultBranch(ctx context.Context, repo string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(r.APIBase, "/")+"/repos/"+repo, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "CottenRouter")
	response, err := r.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("refresh %s: %w", repo, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("refresh %s: GitHub returned HTTP %d", repo, response.StatusCode)
	}
	var metadata struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(response.Body).Decode(&metadata); err != nil {
		return "", fmt.Errorf("refresh %s: %w", repo, err)
	}
	if metadata.DefaultBranch == "" {
		return "", fmt.Errorf("refresh %s: empty default branch", repo)
	}
	return metadata.DefaultBranch, nil
}

func (r Resolver) verifyInstaller(ctx context.Context, project Project) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, project.InstallerURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "CottenRouter")
	response, err := r.Client.Do(req)
	if err != nil {
		return fmt.Errorf("verify %s installer: %w", project.Name, err)
	}
	response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 400 {
		return fmt.Errorf("verify %s installer: HTTP %d", project.Name, response.StatusCode)
	}
	return nil
}

func applyBranch(project *Project, branch, rawBase string) {
	project.DefaultBranch = branch
	project.Repository = "https://github.com/" + project.RepoFullName
	project.InstallerURL = strings.TrimRight(rawBase, "/") + "/" + project.RepoFullName + "/" + branch + "/" + project.InstallerPath
	switch project.CommandStyle {
	case BashProcessSubstitution:
		project.InstallCommand = "bash <(curl -fsSL " + project.InstallerURL + ")"
	case PipeToSudoBash:
		project.InstallCommand = "curl -fsSL " + project.InstallerURL + " | sudo bash"
	}
}
