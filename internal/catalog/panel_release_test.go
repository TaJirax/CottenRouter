package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPanelReleaseResolutionMatchesHostArchitectureAndStableChannel(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			var releases []map[string]any
			for _, tag := range []string{"panel-v0.14.4", "panel-v0.9.9", "panel-v0.15.0", "panel-v0.16.0", "v99.0.0"} {
				asset := "soar-panel-" + strings.TrimPrefix(tag, "panel-v") + "-linux-" + arch + ".tar.gz"
				release := map[string]any{"tag_name": tag, "assets": []map[string]string{{"name": asset}, {"name": asset + ".sha256"}}}
				if tag == "panel-v0.15.0" {
					release["prerelease"] = true
				}
				if tag == "panel-v0.16.0" {
					release["draft"] = true
				}
				releases = append(releases, release)
			}
			body, _ := json.Marshal(releases)
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				response := &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}
				switch {
				case strings.HasSuffix(request.URL.Path, "/releases"):
					if request.Header.Get("Cache-Control") != "no-cache" {
						t.Fatal("release discovery was cached")
					}
					response.Body = io.NopCloser(strings.NewReader(string(body)))
				case strings.HasSuffix(request.URL.Path, "/commits/panel-v0.14.4"):
					response.Body = io.NopCloser(strings.NewReader(`{"sha":"7889645568ea5b430a6c64e0cc83da01a2454201"}`))
				case request.Method == http.MethodHead && strings.HasSuffix(request.URL.Path, "/7889645568ea5b430a6c64e0cc83da01a2454201/panel-setup.sh"):
				default:
					return nil, fmt.Errorf("unexpected request %s", request.URL)
				}
				return response, nil
			})}
			project := Project{ID: "whitedns-panel", RepoFullName: "WhiteDNS/Soar-Server", InstallerPath: "panel-setup.sh", CommandStyle: WhiteDNSPanelBundle}
			got, err := (Resolver{Client: client, APIBase: "https://unit.test", RawBase: "https://raw.test"}).resolvePanelForArch(context.Background(), project, "main", arch)
			if err != nil || got.ReleaseTag != "panel-v0.14.4" || got.CommitSHA != "7889645568ea5b430a6c64e0cc83da01a2454201" {
				t.Fatalf("platform release = %+v, %v", got, err)
			}
		})
	}
}

func TestPanelReleaseDoesNotFallbackAcrossArchitecturesOrMissingChecksums(t *testing.T) {
	for _, assets := range []string{
		`[{"name":"soar-panel-0.14.4-linux-amd64.tar.gz"},{"name":"soar-panel-0.14.4-linux-amd64.tar.gz.sha256"}]`,
		`[{"name":"soar-panel-0.14.4-linux-arm64.tar.gz"}]`,
	} {
		client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`[{"tag_name":"panel-v0.14.4","assets":` + assets + `}]`))}, nil
		})}
		_, err := (Resolver{Client: client, APIBase: "https://unit.test"}).resolvePanelForArch(context.Background(), Project{RepoFullName: "WhiteDNS/Soar-Server"}, "main", "arm64")
		if err == nil || !strings.Contains(err.Error(), "arm64 archive and checksum") {
			t.Fatalf("incomplete platform release accepted: %v", err)
		}
	}
}
