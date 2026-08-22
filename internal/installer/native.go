package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/TaJirax/CottenRouter/internal/catalog"
)

const (
	manifestVersion  = 1
	projectStateDir  = "/var/lib/cottenrouter/projects"
	upstreamStateDir = "/var/lib/cottenrouter/upstreams"
)

// installManifest records the immutable upstream installer that was actually
// executed. Purge uses the same script instead of trusting a newer mutable URL.
type installManifest struct {
	Version       int       `json:"version"`
	ProjectID     string    `json:"project_id"`
	Repository    string    `json:"repository"`
	DefaultBranch string    `json:"default_branch"`
	CommitSHA     string    `json:"commit_sha"`
	InstallerURL  string    `json:"installer_url"`
	InstallerFile string    `json:"installer_file"`
	SHA256        string    `json:"sha256"`
	Completed     bool      `json:"completed"`
	RecordedAt    time.Time `json:"recorded_at"`
}

func manifestFile(projectID string) (string, error) {
	if _, ok := FindSpec(projectID); !ok {
		return "", fmt.Errorf("unknown project %q", projectID)
	}
	return filepath.Join(projectStateDir, projectID+".json"), nil
}

func pendingManifestFile(projectID string) (string, error) {
	path, err := manifestFile(projectID)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(path, ".json") + ".pending.json", nil
}

func upstreamInstallerFile(projectID, commit string) (string, error) {
	if _, ok := FindSpec(projectID); !ok {
		return "", fmt.Errorf("unknown project %q", projectID)
	}
	if len(commit) < 40 || len(commit) > 64 {
		return "", fmt.Errorf("invalid upstream commit ID")
	}
	if _, err := hex.DecodeString(commit); err != nil {
		return "", fmt.Errorf("invalid upstream commit ID")
	}
	return filepath.Join(upstreamStateDir, projectID, commit+".sh"), nil
}

func recordUpstreamInstaller(project catalog.Project, installer []byte, completed bool) error {
	installerPath, err := upstreamInstallerFile(project.ID, project.CommitSHA)
	if err != nil {
		return err
	}
	if err := atomicWrite(installerPath, installer, 0700); err != nil {
		return fmt.Errorf("record immutable upstream installer: %w", err)
	}
	digest := sha256.Sum256(installer)
	manifest := installManifest{
		Version: manifestVersion, ProjectID: project.ID, Repository: project.Repository,
		DefaultBranch: project.DefaultBranch, CommitSHA: project.CommitSHA,
		InstallerURL: project.InstallerURL, InstallerFile: installerPath,
		SHA256: hex.EncodeToString(digest[:]), Completed: completed, RecordedAt: time.Now().UTC(),
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	path, err := pendingManifestFile(project.ID)
	if completed {
		path, err = manifestFile(project.ID)
	}
	if err != nil {
		return err
	}
	if err := atomicWrite(path, append(data, '\n'), 0640); err != nil {
		return err
	}
	if completed {
		pending, _ := pendingManifestFile(project.ID)
		if err := os.Remove(pending); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove pending install manifest: %w", err)
		}
	}
	return nil
}

func discardPendingInstallRecord(projectID string) {
	path, err := pendingManifestFile(projectID)
	if err != nil {
		return
	}
	data, readErr := os.ReadFile(path)
	var pending installManifest
	if readErr == nil && json.Unmarshal(data, &pending) == nil {
		expected, pathErr := upstreamInstallerFile(projectID, pending.CommitSHA)
		if pathErr == nil && filepath.Clean(pending.InstallerFile) == expected {
			keep := false
			if completedPath, completedErr := manifestFile(projectID); completedErr == nil {
				if completedData, completedReadErr := os.ReadFile(completedPath); completedReadErr == nil {
					var completed installManifest
					if json.Unmarshal(completedData, &completed) == nil && filepath.Clean(completed.InstallerFile) == expected {
						keep = true
					}
				}
			}
			if !keep {
				_ = os.Remove(expected)
				_ = os.Remove(filepath.Dir(expected))
			}
		}
	}
	_ = os.Remove(path)
}

// loadInstallManifest returns the completed install record, falling back to the
// pending record left by an install whose upstream script already ran. A failed
// install still put files on the host, so its exact pinned uninstaller is the
// only safe way to remove them.
func loadInstallManifest(projectID string) (installManifest, error) {
	manifest, err := loadInstallManifestFile(projectID, manifestFile)
	if err == nil || !os.IsNotExist(err) {
		return manifest, err
	}
	return loadInstallManifestFile(projectID, pendingManifestFile)
}

func loadInstallManifestFile(projectID string, locate func(string) (string, error)) (installManifest, error) {
	path, err := locate(projectID)
	if err != nil {
		return installManifest{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return installManifest{}, err
	}
	var manifest installManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return installManifest{}, fmt.Errorf("decode install manifest: %w", err)
	}
	if manifest.Version != manifestVersion || manifest.ProjectID != projectID {
		return installManifest{}, fmt.Errorf("invalid install manifest for %s", projectID)
	}
	installerPath, err := upstreamInstallerFile(projectID, manifest.CommitSHA)
	if err != nil || filepath.Clean(manifest.InstallerFile) != installerPath {
		return installManifest{}, fmt.Errorf("unsafe installer path in %s manifest", projectID)
	}
	installer, err := os.ReadFile(installerPath)
	if err != nil {
		return installManifest{}, fmt.Errorf("read recorded upstream installer: %w", err)
	}
	digest := sha256.Sum256(installer)
	if hex.EncodeToString(digest[:]) != manifest.SHA256 {
		return installManifest{}, fmt.Errorf("recorded upstream installer checksum mismatch")
	}
	return manifest, nil
}

func (m Manager) runNativeUninstall(ctx context.Context, spec Spec) error {
	if spec.Kind == ConfigSlipGate {
		slipgateBin, err := resolveSlipGateBinary(spec.WorkDir)
		if err != nil {
			return err
		}
		return m.runProtectedCommand(ctx, spec, slipgateBin, []string{"uninstall"}, spec.WorkDir)
	}
	manifest, err := loadInstallManifest(spec.ID)
	if err != nil {
		return fmt.Errorf("cannot run matching native uninstaller: %w", err)
	}
	return m.runProtectedCommand(ctx, spec, "bash", []string{manifest.InstallerFile, "--uninstall"}, spec.WorkDir)
}

func removeInstallRecord(projectID string) error {
	manifest, err := loadInstallManifest(projectID)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if removeErr := os.Remove(manifest.InstallerFile); removeErr != nil && !os.IsNotExist(removeErr) {
			return removeErr
		}
		_ = os.Remove(filepath.Dir(manifest.InstallerFile))
	}
	path, pathErr := manifestFile(projectID)
	if pathErr != nil {
		return pathErr
	}
	if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	pending, pendingErr := pendingManifestFile(projectID)
	if pendingErr != nil {
		return pendingErr
	}
	if removeErr := os.Remove(pending); removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return nil
}
