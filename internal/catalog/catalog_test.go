package catalog

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestLatestUsesCurrentDefaultBranchesAndVerifiesInstallers(t *testing.T) {
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/releases") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"tag_name":"panel-v0.5.1","assets":[{"name":"soar-panel-0.5.1-linux-amd64.tar.gz"},{"name":"soar-panel-0.5.1-linux-amd64.tar.gz.sha256"}]}]`)), Header: make(http.Header)}, nil
		}
		if request.Method == http.MethodGet && (strings.HasSuffix(request.URL.Path, "/commits/stable") || strings.HasSuffix(request.URL.Path, "/commits/v0.1.3") || strings.HasSuffix(request.URL.Path, "/commits/panel-v0.5.1")) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"sha":"0123456789abcdef0123456789abcdef01234567"}`)), Header: make(http.Header)}, nil
		}
		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"default_branch":"stable"}`)), Header: make(http.Header)}, nil
		}
		if request.Method == http.MethodHead && strings.Contains(request.URL.Path, "/0123456789abcdef0123456789abcdef01234567/") {
			return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 404, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	projects, err := (Resolver{Client: client, APIBase: "https://unit.test", RawBase: "https://raw.test"}).Latest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 8 {
		t.Fatalf("got %d projects", len(projects))
	}
	for _, project := range projects {
		if project.DefaultBranch != "stable" || project.CommitSHA != "0123456789abcdef0123456789abcdef01234567" || !strings.Contains(project.InstallerURL, "/0123456789abcdef0123456789abcdef01234567/") || !strings.Contains(project.InstallCommand, "0123456789abcdef0123456789abcdef01234567") {
			t.Fatalf("project was not refreshed: %+v", project)
		}
		if project.CommandStyle == SourceGo && project.InstallCommand != "go install github.com/getlantern/soar/cmd/soar-server@0123456789abcdef0123456789abcdef01234567" {
			t.Fatalf("source build was not pinned: %q", project.InstallCommand)
		}
	}
}

func TestLatestProjectRefreshesOnlyRequestedRepository(t *testing.T) {
	apiGets := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodGet {
			apiGets++
			if strings.HasSuffix(request.URL.Path, "/commits/main") {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"sha":"abcdef0123456789abcdef0123456789abcdef01"}`)), Header: make(http.Header)}, nil
			}
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"default_branch":"main"}`)), Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	project, err := (Resolver{Client: client, APIBase: "https://unit.test", RawBase: "https://raw.test"}).LatestProject(context.Background(), "thefeed")
	if err != nil {
		t.Fatal(err)
	}
	if project.ID != "thefeed" || apiGets != 2 {
		t.Fatalf("resolved %+v with %d API GETs", project, apiGets)
	}
}

func TestSoarBuildUsesVerifiedGoModuleAtResolvedCommit(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	verified := false
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body io.ReadCloser = http.NoBody
		switch request.Method + " " + request.URL.Path {
		case "GET /repos/getlantern/soar":
			body = io.NopCloser(strings.NewReader(`{"default_branch":"main"}`))
		case "GET /repos/getlantern/soar/commits/main":
			body = io.NopCloser(strings.NewReader(`{"sha":"` + sha + `"}`))
		case "HEAD /getlantern/soar/" + sha + "/go.mod":
			verified = true
		default:
			return &http.Response{StatusCode: 404, Body: body, Header: make(http.Header)}, nil
		}
		return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
	})}
	project, err := (Resolver{Client: client, APIBase: "https://unit.test", RawBase: "https://raw.test"}).LatestProject(context.Background(), "soar")
	if err != nil {
		t.Fatal(err)
	}
	if !verified || project.CommandStyle != SourceGo || project.InstallCommand != "go install github.com/getlantern/soar/cmd/soar-server@"+sha {
		t.Fatalf("unverified or unpinned Soar source build: %+v (verified=%v)", project, verified)
	}
	if !project.Routable || project.Service != "soar-server" || project.DefaultBackend != "127.0.0.1:5305" {
		t.Fatalf("incorrect Soar integration metadata: %+v", project)
	}
}

func TestWhiteDNSCompatibilityResolvesItsReleaseTagInsteadOfFutureMain(t *testing.T) {
	const sha = "abcdef0123456789abcdef0123456789abcdef01"
	verified := false
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body io.ReadCloser = http.NoBody
		switch request.Method + " " + request.URL.Path {
		case "GET /repos/WhiteDNS/Soar-Server":
			body = io.NopCloser(strings.NewReader(`{"default_branch":"future"}`))
		case "GET /repos/WhiteDNS/Soar-Server/commits/v0.1.3":
			body = io.NopCloser(strings.NewReader(`{"sha":"` + sha + `"}`))
		case "HEAD /WhiteDNS/Soar-Server/" + sha + "/setup.sh":
			verified = true
		default:
			t.Fatalf("compatibility installer followed an unpinned revision: %s", request.URL)
		}
		return &http.Response{StatusCode: 200, Body: body, Header: make(http.Header)}, nil
	})}
	project, err := (Resolver{Client: client, APIBase: "https://unit.test", RawBase: "https://raw.test"}).LatestProject(context.Background(), "soar22")
	if err != nil || !verified || project.CommitSHA != sha || project.PinnedRef != "v0.1.3" {
		t.Fatalf("older installer could drift to a newer engine: %+v, %v", project, err)
	}
}
