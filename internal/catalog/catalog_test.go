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
		if request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/repos/") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"default_branch":"stable"}`)), Header: make(http.Header)}, nil
		}
		if request.Method == http.MethodHead && strings.Contains(request.URL.Path, "/stable/") {
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
		if project.DefaultBranch != "stable" || !strings.Contains(project.InstallCommand, "/stable/") {
			t.Fatalf("project was not refreshed: %+v", project)
		}
	}
}
