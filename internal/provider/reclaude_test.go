package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

func TestReClaudeConfigDirFollowsTheCurrentAccount(t *testing.T) {
	limitsHome(t)
	home, _ := os.UserHomeDir()
	reClaudeRoot := filepath.Join(home, ".rec", "claude")
	if got := ReClaudeConfigDir(); got != "" {
		t.Fatalf("no ReClaude folder: %q, want unknown", got)
	}
	for _, accountID := range []string{"54786", "54793"} {
		if err := os.MkdirAll(filepath.Join(reClaudeRoot, "accounts", accountID, "config"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, scenario := range []struct{ current, want string }{
		{"54786\n", filepath.Join(reClaudeRoot, "accounts", "54786", "config")},
		{"54793", filepath.Join(reClaudeRoot, "accounts", "54793", "config")},
		{"99999", ""},
		{"..", ""},
		{"../54786", ""},
		{"", ""},
	} {
		if err := os.WriteFile(filepath.Join(reClaudeRoot, "current"), []byte(scenario.current), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := ReClaudeConfigDir(); got != scenario.want {
			t.Errorf("current %q: %q, want %q", scenario.current, got, scenario.want)
		}
	}
}

func TestReClaudeStatusChecksLiveDaemonHealth(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	limitsHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	launcherDirectory := t.TempDir()
	testenv.Program(t, filepath.Join(launcherDirectory, "reclaude"), `#!/bin/sh
if [ "$1" != status ]; then exit 2; fi
if [ "$2" != --check ] || [ "$3" != --json ]; then
  printf '[global]\nBackground service: recorded as running (unverified)\n'
  exit 0
fi
printf '{"checked":%s,"global":{"daemon":{"status":{"state":"%s"},"recorded_running":true}}}\n' "$FAKE_RECLAUDE_CHECKED" "$FAKE_RECLAUDE_HEALTH"
if [ "$FAKE_RECLAUDE_HEALTH" = unreachable ]; then exit 2; fi
`)
	t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_RECLAUDE_CHECKED", "true")
	t.Setenv("FAKE_RECLAUDE_HEALTH", "reachable")
	if state := ReClaudeStatus(); !state.On || !state.Ready {
		t.Fatalf("reachable checked daemon must be ready: %+v", state)
	}
	t.Setenv("FAKE_RECLAUDE_HEALTH", "unreachable")
	if _, err := ReClaudeBinary(t.Context()); !errors.Is(err, errReClaudeDaemonDown) {
		t.Fatalf("recorded_running must not hide an unreachable daemon: %v", err)
	}
	t.Setenv("FAKE_RECLAUDE_HEALTH", "unverified")
	if state := ReClaudeStatus(); state.Ready || !strings.Contains(state.Why, "did not report verified") {
		t.Fatalf("unverified health must remain unknown: %+v", state)
	}
	t.Setenv("FAKE_RECLAUDE_CHECKED", "false")
	t.Setenv("FAKE_RECLAUDE_HEALTH", "reachable")
	if state := ReClaudeStatus(); state.Ready || !strings.Contains(state.Why, "did not verify") {
		t.Fatalf("unchecked snapshot must not be accepted as live health: %+v", state)
	}
}

func TestReClaudeStatusFallsBackToLegacyLauncher(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	limitsHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	launcherDirectory := t.TempDir()
	testenv.Program(t, filepath.Join(launcherDirectory, "reclaude"), `#!/bin/sh
if [ "$1" != status ]; then exit 2; fi
if [ "$2" = --check ]; then exit 2; fi
printf 'daemon_running: true\ndaemon_reachable: true\n'
`)
	t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	if state := ReClaudeStatus(); !state.Ready {
		t.Fatalf("legacy launcher must remain usable: %+v", state)
	}
}

func TestReClaudeProbeHonorsCallerCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	limitsHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	launcherDirectory := t.TempDir()
	testenv.Program(t, filepath.Join(launcherDirectory, "reclaude"), "#!/bin/sh\nprintf 'daemon_running: true\\ndaemon_reachable: true\\n'\n")
	t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	requestContext, cancel := context.WithCancel(context.Background())
	cancel()
	account := reClaudeAccount().Account
	if _, _, err := account.Token(requestContext); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled caller must cancel the daemon probe, got %v", err)
	}
}

func TestReClaudeExclusivelyReplacesClaudeAccounts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	limitsHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	launcherDir := t.TempDir()
	statusScript := "#!/bin/sh\nif [ \"$1\" = status ]; then printf 'daemon_running: true\\ndaemon_reachable: true\\n'; exit 0; fi\nexit 1\n"
	if err := os.WriteFile(filepath.Join(launcherDir, "reclaude"), []byte(statusScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", launcherDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	originalLogins := []savedLogin{{Agent: "claude", User: "personal@example.com", Auth: []byte(`{"oauth":{"accessToken":"original"}}`), On: true}}
	if err := writeLogins(originalLogins); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(loginsPath())
	if err != nil {
		t.Fatal(err)
	}
	if state := ReClaudeStatus(); state.On || state.Ready {
		t.Fatalf("ReClaude must be off by default: %+v", state)
	}
	if err := SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	if state := ReClaudeStatus(); !state.On || !state.Ready {
		t.Fatalf("synthetic account must use the ready launcher: %+v", state)
	}
	keepLast(SubscriptionQuota{Provider: "claude", User: reClaudeUser, Windows: []QuotaWindow{{Name: "5 hours", Used: 0.25}}}, "")
	failedRead := keepLast(SubscriptionQuota{Provider: "claude", User: reClaudeUser, Windows: []QuotaWindow{}, Error: "context deadline exceeded"}, "")
	if failedRead.Error == "" || len(failedRead.Windows) != 0 || failedRead.AsOf != nil {
		t.Fatalf("a failed ReClaude read borrowed stale quota: %+v", failedRead)
	}
	var restored settings.Settings
	restored.KeepOwn(settings.Load())
	if !restored.ReClaude {
		t.Fatal("restoring settings from another computer lost the local source choice")
	}
	claudeAccounts := []Provider{}
	for _, account := range Accounts() {
		if account.ID == "claude" {
			claudeAccounts = append(claudeAccounts, account)
		}
	}
	if len(claudeAccounts) != 1 || !claudeAccounts[0].Account.ReClaude || len(claudeAccounts[0].AlsoOn()) != 0 {
		t.Fatalf("ReClaude must be the only Claude candidate: %+v", claudeAccounts)
	}
	if logins := Logins("claude"); len(logins) != 1 || logins[0].User != reClaudeUser {
		t.Fatalf("personal sign-ins must not be listed: %+v", logins)
	}
	if _, _, err := (&Account{Agent: "claude", User: "personal@example.com"}).Token(context.Background()); err == nil {
		t.Fatal("a stale personal candidate remained usable")
	}
	if _, err := StartSignIn("claude"); err == nil {
		t.Fatal("sign-in must not replace ReClaude's managed Claude config")
	}
	if err := SwitchLogin("claude", "personal@example.com"); err == nil {
		t.Fatal("the saved personal sign-in must not be switched in")
	}
	if after, err := os.ReadFile(loginsPath()); err != nil || string(after) != string(before) {
		t.Fatalf("existing login file changed: %v", err)
	}
	if err := settings.Save(settings.Settings{ReClaude: true, NoAccountDiscovery: true}); err != nil {
		t.Fatal(err)
	}
	ForgetAccounts()
	if accounts := Accounts(); len(accounts) != 0 {
		t.Fatalf("discovery off still exposed accounts: %+v", accounts)
	}
	if state := ReClaudeStatus(); state.Ready || !strings.Contains(state.Why, "subscription accounts") {
		t.Fatalf("discovery off must override the source: %+v", state)
	}
	if err := settings.Save(settings.Settings{ReClaude: true}); err != nil {
		t.Fatal(err)
	}
	if err := SetReClaude(false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claudeAccounts[0].Account.Token(context.Background()); err == nil {
		t.Fatal("a stale ReClaude account stayed usable after the source was turned off")
	}
	if logins := Logins("claude"); len(logins) != 1 || logins[0].User != "personal@example.com" {
		t.Fatalf("original login should reappear: %+v", logins)
	}
}

func TestReClaudeSourceIsUnknownOnUnreadableSettings(t *testing.T) {
	limitsHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	broken := []byte("not a settings document")
	if err := os.WriteFile(settings.Path(), broken, 0o600); err != nil {
		t.Fatal(err)
	}
	ForgetAccounts()
	if _, err := ReClaudeEnabled(); err == nil {
		t.Fatal("an unreadable source setting must be unknown, not off")
	}
	if account, ok := claudeAccount(); ok || account.Account != nil {
		t.Fatal("unreadable settings must not select personal Claude")
	}
	if err := SetReClaude(false); err == nil {
		t.Fatal("an unreadable settings file was overwritten")
	}
	if contents, err := os.ReadFile(settings.Path()); err != nil || string(contents) != string(broken) {
		t.Fatalf("settings changed: %s, %v", contents, err)
	}
}
