package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
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
	CommitSHA      string
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
		applyRef(&projects[i], "main", "main", "https://raw.githubusercontent.com")
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
		resolved, err := r.resolve(ctx, projects[i])
		if err != nil {
			return nil, err
		}
		projects[i] = resolved
	}
	return projects, nil
}

// LatestProject resolves one project without spending API quota refreshing
// unrelated repositories. This matters when the TUI installs several selected
// projects one at a time on GitHub's unauthenticated API limit.
func (r Resolver) LatestProject(ctx context.Context, id string) (Project, error) {
	for _, project := range Projects() {
		if project.ID == id {
			return r.resolve(ctx, project)
		}
	}
	return Project{}, fmt.Errorf("unknown project %q", id)
}

func (r Resolver) resolve(ctx context.Context, project Project) (Project, error) {
	if r.Client == nil {
		r.Client = &http.Client{Timeout: 20 * time.Second}
	}
	if r.APIBase == "" {
		r.APIBase = "https://api.github.com"
	}
	if r.RawBase == "" {
		r.RawBase = "https://raw.githubusercontent.com"
	}
	branch, err := r.defaultBranch(ctx, project.RepoFullName)
	if err != nil {
		return Project{}, err
	}
	commit, err := r.branchCommit(ctx, project.RepoFullName, branch)
	if err != nil {
		return Project{}, err
	}
	applyRef(&project, branch, commit, r.RawBase)
	if err := r.verifyInstaller(ctx, project); err != nil {
		return Project{}, err
	}
	return project, nil
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

func (r Resolver) branchCommit(ctx context.Context, repo, branch string) (string, error) {
	endpoint := strings.TrimRight(r.APIBase, "/") + "/repos/" + repo + "/commits/" + url.PathEscape(branch)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "CottenRouter")
	response, err := r.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", repo, branch, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve %s@%s: GitHub returned HTTP %d", repo, branch, response.StatusCode)
	}
	var metadata struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(response.Body).Decode(&metadata); err != nil {
		return "", fmt.Errorf("resolve %s@%s: %w", repo, branch, err)
	}
	if !regexp.MustCompile(`^[0-9a-fA-F]{40,64}$`).MatchString(metadata.SHA) {
		return "", fmt.Errorf("resolve %s@%s: invalid commit ID", repo, branch)
	}
	return strings.ToLower(metadata.SHA), nil
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

func applyRef(project *Project, branch, ref, rawBase string) {
	project.DefaultBranch = branch
	if ref != branch {
		project.CommitSHA = ref
	}
	project.Repository = "https://github.com/" + project.RepoFullName
	project.InstallerURL = strings.TrimRight(rawBase, "/") + "/" + project.RepoFullName + "/" + ref + "/" + project.InstallerPath
	switch project.CommandStyle {
	case BashProcessSubstitution:
		project.InstallCommand = "bash <(curl -fsSL " + project.InstallerURL + ")"
	case PipeToSudoBash:
		project.InstallCommand = "curl -fsSL " + project.InstallerURL + " | sudo bash"
	}
}
