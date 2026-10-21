package installer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (m Manager) verifyCottenDNSRelease(ctx context.Context, spec Spec, expected string) error {
	unit, err := m.Runner.Output(ctx, "systemctl", "show", spec.Service, "--property=ExecStart", "--value")
	if err != nil {
		return fmt.Errorf("inspect installed CottenDNS executable: %w", err)
	}
	match := regexp.MustCompile(`(?:^|[ {;])path=([^ ;}]+)`).FindSubmatch(unit)
	if len(match) != 2 {
		return fmt.Errorf("cannot verify CottenDNS version: systemd did not report its executable")
	}
	binary := string(match[1])
	if filepath.Dir(binary) != filepath.Clean(spec.WorkDir) {
		return fmt.Errorf("CottenDNS executable is outside its managed directory: %s", binary)
	}
	if err := rejectSymlinkComponents(binary); err != nil {
		return err
	}
	version, err := m.Runner.Output(ctx, binary, "--version")
	if err != nil {
		return fmt.Errorf("read installed CottenDNS version: %w", err)
	}
	if !releaseVersionMatches(string(version), expected) {
		return fmt.Errorf("CottenDNS update installed %q; expected latest WhiteDNS/CottenDNS release %s", string(version), expected)
	}
	capabilities, err := m.Runner.Output(ctx, binary, "--capabilities")
	if err != nil {
		return fmt.Errorf("installed CottenDNS does not advertise Normal/Max capabilities: %w", err)
	}
	features := map[string]bool{}
	for _, value := range strings.Fields(string(capabilities)) {
		features[value] = true
	}
	for _, feature := range []string{"cottendns-normal", "cottenmax", "dns-udp", "dns-tcp", "shared-key"} {
		if !features[feature] {
			return fmt.Errorf("installed CottenDNS is missing %s; install a current server release before connecting Max clients", feature)
		}
	}
	return nil
}

// The native installer may replace ExecStart before its later checks fail.
// Restore the previous unit before restarting the saved configuration.
func cottenDNSUnitRollback(spec Spec) (func(), error) {
	if spec.ID != "cottendns" {
		return func() {}, nil
	}
	path := "/etc/systemd/system/" + spec.Service + ".service"
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	data, err := readSmallFile(path)
	if os.IsNotExist(err) {
		return func() {}, nil
	}
	if err != nil {
		return nil, err
	}
	return func() { _ = atomicWrite(path, data, 0644) }, nil
}
