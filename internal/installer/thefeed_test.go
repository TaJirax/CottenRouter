package installer

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNativeTheFeedDomainsIncludesAllFeatures(t *testing.T) {
	data := []byte("THEFEED_DOMAIN=Feed.Example.\r\n" +
		"THEFEED_EXTRA_DOMAINS=alias.example, feed.example\r\n" +
		"THEFEED_CHAT_DOMAINS=chat.example\r\n" +
		"THEFEED_MTPROTO_DOMAINS=mtproto.example,tunnel.example\r\n" +
		"THEFEED_FUTURE_TRANSPORT_DOMAINS=future.example\r\n" +
		"THEFEED_GITHUB_RELAY_REPO=owner/repository\r\n" +
		"THEFEED_KEY=secret\r\n")
	got, err := nativeTheFeedDomains(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"feed.example", "alias.example", "chat.example", "mtproto.example", "tunnel.example", "future.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("domains = %v, want %v", got, want)
	}
}

func TestNativeTheFeedDomainsUsesEnvironmentFileSyntax(t *testing.T) {
	got, err := nativeTheFeedDomains([]byte("# THEFEED_CHAT_DOMAINS=ignored.example\n" +
		"THEFEED_DOMAIN=old.example\n" +
		"THEFEED_DOMAIN=feed.example\n" +
		" export THEFEED_MTPROTO_DOMAINS = ' MTP.Example., relay.example ' \n" +
		"THEFEED_CHAT_DOMAINS=\"chat.example\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"feed.example", "chat.example", "mtp.example", "relay.example"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("domains = %v, want %v", got, want)
	}
}

func TestNativeTheFeedDomainsRejectsMalformedFeatureZone(t *testing.T) {
	for _, value := range []string{"https://relay.example", "$(hostname)", "bad domain.example"} {
		_, err := nativeTheFeedDomains([]byte("THEFEED_DOMAIN=feed.example\nTHEFEED_MTPROTO_DOMAINS=" + value + "\n"))
		if err == nil || !strings.Contains(err.Error(), "THEFEED_MTPROTO_DOMAINS") {
			t.Fatalf("feature zone %q was not refused with its setting name: %v", value, err)
		}
	}
}

func TestTheFeedConfigurePreservesNativeFeatureSettings(t *testing.T) {
	spec, _ := FindSpec("thefeed")
	const native = "THEFEED_MTPROTO_DOMAINS=mtp.example\n" +
		"THEFEED_MTPROTO_MAX_TUNNELS=100\n" +
		"THEFEED_MTPROTO_MAX_SESSIONS=2000\n" +
		"THEFEED_MTPROTO_REPLY_SESSION_KB=64\n" +
		"THEFEED_MTPROTO_REPLY_BUDGET_MB=128\n" +
		"THEFEED_MTPROTO_ENABLED=0\n" +
		"THEFEED_DNS_MEDIA_ENABLED=1\n" +
		"THEFEED_GITHUB_RELAY_ENABLED=1\n" +
		"THEFEED_GITHUB_RELAY_TOKEN=keep-token\n" +
		"THEFEED_FUTURE_TRANSPORT_DOMAINS=future.example\n"
	got, err := configure(spec, Request{Domain: "feed.example", PrivatePort: 5304}, PortPlan{}, []byte(native))
	if err != nil {
		t.Fatal(err)
	}
	for line := range strings.SplitSeq(strings.TrimSpace(native), "\n") {
		if !strings.Contains(string(got), line+"\n") {
			t.Fatalf("native feature setting changed: %q in %s", line, got)
		}
	}
	domains, err := nativeTheFeedDomains(got)
	if err != nil || !reflect.DeepEqual(domains, []string{"feed.example", "mtp.example", "future.example"}) {
		t.Fatalf("configured feature domains = %v, err %v", domains, err)
	}
}

func TestTheFeedSourceRequirementMatchesNativeServer(t *testing.T) {
	for _, source := range []string{"channels.txt", "private_channels.txt", "x_accounts.txt"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, source), []byte("# comment\n\n@source\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := requireTheFeedSource(dir); err != nil {
				t.Fatalf("native source %s was refused: %v", source, err)
			}
		})
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "thefeed.env"), []byte("THEFEED_CHAT_DOMAINS=chat.example\nTHEFEED_MTPROTO_DOMAINS=mtp.example\nTHEFEED_GITHUB_RELAY_ENABLED=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := requireTheFeedSource(dir); err == nil {
		t.Fatal("native server still requires a source even when chat, MTProto and relay are enabled")
	}
}

func TestTheFeedNativeWizardSelectionsReplacePreviousRequest(t *testing.T) {
	old := Request{ProjectID: "thefeed", Domain: "old.example", ExtraDomains: "removed.example", ChatDomains: "disabled-chat.example", PrivatePort: 5318, RouterConfig: "/custom/router.json"}
	native := []byte("THEFEED_DOMAIN= New.Example.\nTHEFEED_EXTRA_DOMAINS=new-alias.example\nTHEFEED_CHAT_DOMAINS=\n" +
		"THEFEED_MTPROTO_DOMAINS=mtp.example\nTHEFEED_MTPROTO_MAX_SESSIONS=1500\nTHEFEED_DNS_MEDIA_ENABLED=1\n")
	got, err := theFeedNativeRequest(old, native)
	if err != nil {
		t.Fatal(err)
	}
	if got.Domain != "new.example" || got.ExtraDomains != "new-alias.example" || got.ChatDomains != "" || got.PrivatePort != 5318 || got.RouterConfig != "/custom/router.json" {
		t.Fatalf("final native choices were not retained: %+v", got)
	}
	spec, _ := FindSpec("thefeed")
	configured, err := configure(spec, got, PortPlan{}, native)
	if err != nil {
		t.Fatal(err)
	}
	domains, err := nativeTheFeedDomains(configured)
	if err != nil || !reflect.DeepEqual(domains, []string{"new.example", "new-alias.example", "mtp.example"}) {
		t.Fatalf("old update domains resurrected: %v, %v", domains, err)
	}
	for _, setting := range []string{"THEFEED_MTPROTO_MAX_SESSIONS=1500", "THEFEED_DNS_MEDIA_ENABLED=1"} {
		if !strings.Contains(string(configured), setting) {
			t.Fatalf("native feature setting changed: %s", setting)
		}
	}
}

func TestTheFeedNativeWizardCanRemoveAllAliasesAndMessenger(t *testing.T) {
	request := Request{ProjectID: "thefeed", Domain: "feed.example", ExtraDomains: "removed.example", ChatDomains: "removed-chat.example"}
	got, err := theFeedNativeRequest(request, []byte("THEFEED_DOMAIN=feed.example\nTHEFEED_EXTRA_DOMAINS=\nTHEFEED_CHAT_DOMAINS=\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ExtraDomains != "" || got.ChatDomains != "" || got.PrivatePort != 5304 || got.RouterConfig != "/etc/cottenrouter/config.json" {
		t.Fatalf("removed native choices or default private port were lost: %+v", got)
	}
}

func TestTheFeedNativeWizardRejectsInvalidFinalConfig(t *testing.T) {
	for _, data := range []string{
		"THEFEED_DOMAIN=\nTHEFEED_CHAT_DOMAINS=chat.example\n",
		"THEFEED_DOMAIN=one.example,two.example\n",
		"THEFEED_DOMAIN=feed.example\nTHEFEED_MTPROTO_DOMAINS=https://bad.example\n",
	} {
		if _, err := theFeedNativeRequest(Request{ProjectID: "thefeed", Domain: "old.example"}, []byte(data)); err == nil {
			t.Fatalf("invalid native configuration accepted: %s", data)
		}
	}
}
