package installer

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/TaJirax/CottenRouter/internal/dnswire"
)

var theFeedDomainListKey = regexp.MustCompile(`^THEFEED_(?:[A-Z0-9]+_)*DOMAINS$`)
var theFeedDomainName = regexp.MustCompile(`^[a-z0-9_-]+(?:\.[a-z0-9_-]+)*$`)

// nativeTheFeedDomains imports the native DNS surface, including messenger and
// MTProto zones. New native features that follow the *_DOMAINS convention use
// the same opaque DNS backend without requiring another router release.
func nativeTheFeedDomains(data []byte) ([]string, error) {
	values, keys := theFeedDomainValues(data)
	var domains []string
	seen := make(map[string]bool)
	for _, key := range keys {
		normalized, err := normalizeDomainCSV(values[key])
		if err != nil {
			return nil, fmt.Errorf("thefeed %s: %w", key, err)
		}
		for _, domain := range strings.Split(normalized, ",") {
			if domain != "" && !theFeedDomainName.MatchString(domain) {
				return nil, fmt.Errorf("thefeed %s: invalid delegated domain %q", key, domain)
			}
			if domain != "" && !seen[domain] {
				seen[domain] = true
				domains = append(domains, domain)
			}
		}
	}
	return domains, nil
}

// theFeedNativeRequest takes the final native wizard selections, including
// intentional removals. Merging the old update request here would resurrect
// aliases and re-enable messenger domains the operator just disabled.
func theFeedNativeRequest(request Request, data []byte) (Request, error) {
	if request.ProjectID != "thefeed" {
		return Request{}, fmt.Errorf("native thefeed settings require thefeed project")
	}
	if _, err := nativeTheFeedDomains(data); err != nil {
		return Request{}, err
	}
	values, _ := theFeedDomainValues(data)
	domain, err := dnswire.NormalizeDomain(values["THEFEED_DOMAIN"])
	if err != nil || !theFeedDomainName.MatchString(domain) {
		return Request{}, fmt.Errorf("thefeed THEFEED_DOMAIN must contain one valid delegated domain")
	}
	request.Domain = domain
	request.ExtraDomains, err = normalizeDomainCSV(values["THEFEED_EXTRA_DOMAINS"])
	if err != nil {
		return Request{}, fmt.Errorf("thefeed THEFEED_EXTRA_DOMAINS: %w", err)
	}
	request.ChatDomains, err = normalizeDomainCSV(values["THEFEED_CHAT_DOMAINS"])
	if err != nil {
		return Request{}, fmt.Errorf("thefeed THEFEED_CHAT_DOMAINS: %w", err)
	}
	if _, err := request.Validate(); err != nil {
		return Request{}, err
	}
	return request, nil
}

func theFeedDomainValues(data []byte) (map[string]string, []string) {
	values := make(map[string]string)
	keys := []string{"THEFEED_DOMAIN", "THEFEED_EXTRA_DOMAINS", "THEFEED_CHAT_DOMAINS", "THEFEED_MTPROTO_DOMAINS"}
	known := make(map[string]bool)
	for _, key := range keys {
		known[key] = true
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || (key != "THEFEED_DOMAIN" && !theFeedDomainListKey.MatchString(key)) {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
			value = value[1 : len(value)-1]
		}
		// EnvironmentFile uses the last assignment. Never evaluate shell syntax.
		values[key] = value
		if !known[key] {
			known[key] = true
			keys = append(keys, key)
		}
	}
	return values, keys
}
