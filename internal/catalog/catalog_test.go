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
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/commits/stable") {
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
	if len(projects) != 5 {
		t.Fatalf("got %d projects", len(projects))
	}
	for _, project := range projects {
		if project.DefaultBranch != "stable" || project.CommitSHA != "0123456789abcdef0123456789abcdef01234567" || !strings.Contains(project.InstallCommand, "/0123456789abcdef0123456789abcdef01234567/") {
			t.Fatalf("project was not refreshed: %+v", project)
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
