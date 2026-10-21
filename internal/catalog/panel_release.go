package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// Panel releases share a repository with the deliberately older standalone
// installer. Resolve the current published panel channel, never /releases/latest
// or an embedded setup version that can point to the other distribution.
func (r Resolver) resolvePanel(ctx context.Context, project Project, branch string) (Project, error) {
	selected := ""
	for page := 1; page <= 3; page++ {
		url := fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", strings.TrimRight(r.APIBase, "/"), project.RepoFullName, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return Project{}, err
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "CottenRouter")
		req.Header.Set("Cache-Control", "no-cache")
		resp, err := r.Client.Do(req)
		if err != nil {
			return Project{}, fmt.Errorf("resolve current WhiteDNS Panel release: %w", err)
		}
		var releases []struct {
			Tag        string `json:"tag_name"`
			Draft      bool   `json:"draft"`
			Prerelease bool   `json:"prerelease"`
			Assets     []struct {
				Name string `json:"name"`
			} `json:"assets"`
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return Project{}, fmt.Errorf("resolve WhiteDNS Panel releases: HTTP %d", resp.StatusCode)
		}
		err = json.NewDecoder(resp.Body).Decode(&releases)
		resp.Body.Close()
		if err != nil {
			return Project{}, err
		}
		for _, release := range releases {
			if release.Draft || release.Prerelease || !regexp.MustCompile(`^panel-v[0-9]+\.[0-9]+\.[0-9]+$`).MatchString(release.Tag) {
				continue
			}
			asset := "soar-panel-" + strings.TrimPrefix(release.Tag, "panel-v") + "-linux-amd64.tar.gz"
			found := map[string]bool{}
			for _, a := range release.Assets {
				found[a.Name] = true
			}
			if !found[asset] || !found[asset+".sha256"] {
				continue
			}
			if selected == "" || newerPanelTag(release.Tag, selected) {
				selected = release.Tag
			}
		}
		if len(releases) < 100 {
			break
		}
	}
	if selected != "" {
		commit, err := r.branchCommit(ctx, project.RepoFullName, selected)
		if err != nil {
			return Project{}, err
		}
		applyRef(&project, branch, commit, r.RawBase)
		project.ReleaseTag = selected
		if err := r.verifyInstaller(ctx, project); err != nil {
			return Project{}, err
		}
		return project, nil
	}
	return Project{}, fmt.Errorf("no published WhiteDNS Panel release has the amd64 archive and checksum; existing service was not changed")
}

func newerPanelTag(a, b string) bool {
	x, y := strings.Split(strings.TrimPrefix(a, "panel-v"), "."), strings.Split(strings.TrimPrefix(b, "panel-v"), ".")
	for i := range 3 {
		u, e := strconv.ParseUint(x[i], 10, 32)
		if e != nil {
			return false
		}
		v, e := strconv.ParseUint(y[i], 10, 32)
		if e != nil {
			return true
		}
		if u != v {
			return u > v
		}
	}
	return false
}
