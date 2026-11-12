package gateway

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/provider"
	"github.com/yetone/magpie/internal/settings"
	"github.com/yetone/magpie/internal/testenv"
)

func TestReClaudeToolResultsRecheckSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	for _, scenario := range []struct {
		name      string
		settings  settings.Settings
		wantError string
	}{
		{"daemon stopped", settings.Settings{ReClaude: true}, "daemon"},
		{"source disabled", settings.Settings{}, "off"},
		{"discovery disabled", settings.Settings{ReClaude: true, NoAccountDiscovery: true}, "subscription accounts"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			fresh(t)
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			launcherDirectory := t.TempDir()
			testenv.Program(t, filepath.Join(launcherDirectory, "reclaude"), "#!/bin/sh\nprintf 'daemon_running: false\\ndaemon_reachable: false\\n'\n")
			t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := settings.Save(scenario.settings); err != nil {
				t.Fatal(err)
			}
			server := New()
			t.Cleanup(server.subscription.abortAll)
			pendingResult := make(chan mcpToolResult, 1)
			run := &subscriptionRun{
				bridge: server.subscription, token: "reclaude-test-run", model: "claude-sonnet-5",
				pending: map[string]chan mcpToolResult{"echo-call": pendingResult},
			}
			reclaudeOwner := "claude\x00" + provider.ReClaudeUser + "\x00reclaude"
			run.owner.Store(&reclaudeOwner)
			// If the check is bypassed, end the fake reply instead of hanging.
			run.resume = func() {
				run.emit(Event{Kind: KError, Text: "unexpected resumed CLI"})
				run.finish()
			}
			server.subscription.runs[run.token] = run
			server.subscription.calls["echo-call"] = run
			account := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: provider.ReClaudeUser, ReClaude: true}}
			body := `{"model":"claude-sonnet-5","max_tokens":32,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"echo-call","name":"echo","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"echo-call","content":"fixture result"}]}]}`
			response := httptest.NewRecorder()
			request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
			status, message := server.serveClaudeSubscription(response, request, provider.Anthropic, account, "claude-sonnet-5", []byte(body), &Usage{})
			if status != 502 || !strings.Contains(message, scenario.wantError) {
				t.Errorf("unavailable source resumed a tool result: status=%d error=%q", status, message)
			}
			select {
			case _, delivered := <-pendingResult:
				if delivered {
					t.Error("tool result reached a CLI whose source is no longer available")
				}
			default:
			}
		})
	}
}

func TestReClaudeQueuedRunsRecheckSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	for _, startTurn := range []bool{true, false} {
		name := "one-off"
		if startTurn {
			name = "conversation"
		}
		t.Run(name, func(t *testing.T) {
			fresh(t)
			freshRoom(t, 1)
			t.Setenv("MAGPIE_NO_ACCOUNTS", "")
			launcherDirectory := t.TempDir()
			launcherPath := filepath.Join(launcherDirectory, "reclaude")
			markerPath := filepath.Join(launcherDirectory, "started")
			t.Setenv("FAKE_RECLAUDE_MARKER", markerPath)
			testenv.Program(t, launcherPath, "#!/bin/sh\nif [ \"$1\" = status ]; then printf 'daemon_running: true\\ndaemon_reachable: true\\n'; exit 0; fi\nprintf started > \"$FAKE_RECLAUDE_MARKER\"\nexit 9\n")
			t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
			if err := provider.SetReClaude(true); err != nil {
				t.Fatal(err)
			}
			requestContext, cancel := context.WithCancel(provider.WithReClaude(t.Context()))
			defer cancel()
			release, err := claudeRoom.take(requestContext, "test holding the only slot")
			if err != nil {
				t.Fatal(err)
			}
			releaseSlot := sync.OnceFunc(func() { release(false) })
			server := New()
			t.Cleanup(server.subscription.abortAll)
			finished := make(chan struct{})
			var launchError error
			go func() {
				defer close(finished)
				if startTurn {
					_, _, launchError = server.subscription.startRun(requestContext, &Request{}, "claude-sonnet-5", "", "claude\x00ReClaude\x00reclaude", nil)
				} else {
					launchError = runClaudeTree(requestContext, proc.CommandContext(requestContext, launcherPath, "-p"), "a queued ReClaude probe")
				}
			}()
			defer func() { cancel(); releaseSlot(); <-finished }()
			deadline := time.After(10 * time.Second)
			for {
				claudeRoom.mu.Lock()
				waiting := claudeRoom.waiting
				claudeRoom.mu.Unlock()
				if waiting > 0 {
					break
				}
				select {
				case <-finished:
					t.Fatalf("run did not wait for the occupied slot: %v", launchError)
				case <-deadline:
					t.Fatal("run never reached the occupied slot")
				case <-time.After(5 * time.Millisecond):
				}
			}
			if err := provider.SetReClaude(false); err != nil {
				t.Fatal(err)
			}
			releaseSlot()
			<-finished
			if launchError == nil || !strings.Contains(launchError.Error(), "off") {
				t.Fatalf("source disabled while queued: %v", launchError)
			}
			if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
				t.Fatalf("a disabled source launched a process: %v", err)
			}
		})
	}
}

func TestReClaudeBridgeUsesLocalLauncherAndNeverFallsBackToClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	launcherDirectory := t.TempDir()
	testenv.Program(t, filepath.Join(launcherDirectory, "claude"), "#!/bin/sh\nexit 41\n")
	launcher := `#!/bin/sh
if [ "$1" = status ]; then
  if [ "$FAKE_RECLAUDE_DOWN" = 1 ]; then
    printf 'daemon_running: false\ndaemon_reachable: false\n'
  else
    printf 'daemon_running: true\ndaemon_reachable: true\n'
  fi
  exit 0
fi
if [ -n "$ANTHROPIC_BASE_URL" ] || [ -n "$CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC" ]; then exit 42; fi
if [ "$2" = /usage ]; then
  echo '{"type":"result","is_error":false,"result":"Current session: 25% used · resets Oct 22 at 9:19pm (Australia/Sydney)"}'
  exit 0
fi
if [ "$3" = json ]; then
  echo '{"type":"result","is_error":false,"result":"from reclaude"}'
  exit 0
fi
while IFS= read -r request; do
  echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","usage":{"input_tokens":1}}}}'
  echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"from reclaude"}}}'
  echo '{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}'
  echo '{"type":"stream_event","event":{"type":"message_stop"}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":"from reclaude"}'
done
`
	testenv.Program(t, filepath.Join(launcherDirectory, "reclaude"), launcher)
	t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:1")
	if err := provider.SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.SetReClaude(false) })
	selected, err := claudeBinaryFor(provider.WithReClaude(context.Background()))
	if err != nil || selected.path != filepath.Join(launcherDirectory, "reclaude") {
		t.Fatalf("wrong CLI: %s, %v", selected.path, err)
	}
	launcherContext := provider.WithReClaude(context.Background())
	environment, err := selected.proxied(launcherContext, cleanClaudeEnv(os.Environ()))
	if err != nil {
		t.Fatal(err)
	}
	for _, variable := range []string{"ANTHROPIC_BASE_URL=", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="} {
		if strings.Contains(strings.Join(environment, "\n"), variable) {
			t.Errorf("ReClaude inherited conflicting variable %s", variable)
		}
	}
	if err := askClaude(launcherContext, "", "claude-sonnet-5"); err != nil {
		t.Fatalf("model probe used the wrong launcher: %v", err)
	}
	if report, err := claudeUsage(launcherContext, ""); err != nil || !strings.Contains(report, "25% used") {
		t.Fatalf("allowance read did not use the ReClaude launcher: %s, %v", report, err)
	}
	// Use the real bridge with a fake launcher so the input format and the
	// output event parsing, not just binary selection, are exercised.
	runsEndWithTest(t, launcherDirectory)
	server := New()
	t.Cleanup(server.subscription.abortAll)
	account := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: provider.ReClaudeUser, ReClaude: true}}
	body := `{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	recorder := httptest.NewRecorder()
	var responseUsage Usage
	request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body))
	status, responseError := server.serveClaudeSubscription(recorder, request, provider.Anthropic, account, "claude-sonnet-5", []byte(body), &responseUsage)
	if status != 200 || !strings.Contains(recorder.Body.String(), "from reclaude") {
		t.Fatalf("bridge returned %d, %s: %s", status, responseError, recorder.Body.String())
	}
	for _, requestBody := range []struct {
		protocol provider.Protocol
		body     string
	}{
		{provider.Chat, `{"model":"claude-sonnet-5","messages":[{"role":"user","content":"hi"}],"stream":true}`},
		{provider.Responses, `{"model":"claude-sonnet-5","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}],"stream":true}`},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest("POST", "/", strings.NewReader(requestBody.body))
		status, responseError := server.serveClaudeSubscription(response, request, requestBody.protocol, account, "claude-sonnet-5", []byte(requestBody.body), &responseUsage)
		if status != 200 || !strings.Contains(response.Body.String(), "from reclaude") {
			t.Errorf("%s: %d %s: %s", requestBody.protocol, status, responseError, response.Body.String())
		}
	}
	t.Setenv("FAKE_RECLAUDE_DOWN", "1")
	failed := httptest.NewRecorder()
	status, responseError = server.serveClaudeSubscription(failed, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, account, "claude-sonnet-5", []byte(body), &responseUsage)
	if status == 200 || !strings.Contains(responseError, "daemon") {
		t.Fatalf("a stopped daemon must refuse even an existing run: %d %s", status, responseError)
	}
}

// A ReClaude run's Claude Code keeps its session in the config folder the
// launcher gives it, ~/.rec/claude/accounts/<current>/config, not in
// ~/.claude: looked for there, a turn after an interrupted tool call found
// no session to branch off and was told the whole conversation anew.
func TestReClaudeRunKeepsItsSessionInTheLaunchersConfigDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	home, _ := os.UserHomeDir()
	reClaudeConfigDir := filepath.Join(home, ".rec", "claude", "accounts", "54787", "config")
	if err := os.MkdirAll(reClaudeConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".rec", "claude", "current"), []byte("54787\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcherDirectory := t.TempDir()
	testenv.Program(t, filepath.Join(launcherDirectory, "reclaude"), `#!/bin/sh
if [ "$1" = status ]; then printf 'daemon_running: true\ndaemon_reachable: true\n'; exit 0; fi
while IFS= read -r request; do
  echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","usage":{"input_tokens":1}}}}'
  echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"from reclaude"}}}'
  echo '{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}'
  echo '{"type":"stream_event","event":{"type":"message_stop"}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":"from reclaude"}'
done
`)
	t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := provider.SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.SetReClaude(false) })
	runsEndWithTest(t, launcherDirectory)
	server := New()
	t.Cleanup(server.subscription.abortAll)
	request := &Request{Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Parts: []Part{{Kind: Text, Text: "hi"}}}}}
	run, _, err := server.subscription.startRun(provider.WithReClaude(t.Context()), request, "claude-sonnet-5", "", "claude\x00"+provider.ReClaudeUser+"\x00reclaude", nil)
	if err != nil {
		t.Fatalf("a ReClaude run: %v", err)
	}
	run.mu.Lock()
	sessionDirectories := slices.Clone(run.sessions)
	run.mu.Unlock()
	if len(sessionDirectories) == 0 {
		t.Fatal("the ReClaude run kept no session folder")
	}
	for _, directory := range sessionDirectories {
		if !strings.HasPrefix(directory, filepath.Join(reClaudeConfigDir, "projects")+string(filepath.Separator)) {
			t.Errorf("the ReClaude run looks for its session in %s, not under %s", directory, reClaudeConfigDir)
		}
	}
}

// With ReClaude's config folder known, its runs are still -p: in the
// interactive mode a turn whose tool results found no run waiting started
// a new Claude Code each time (30-86s to the first token on 11-12), and its
// thinking came back with no text. tuiEnv still tells the helper the
// folder, for a run that is the interactive mode by another way.
func TestReClaudeRunsAsPrintWithItsConfigDirKnown(t *testing.T) {
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	was := claudeTUIRuns
	claudeTUIRuns = true
	t.Cleanup(func() { claudeTUIRuns = was })
	t.Cleanup(func() { _ = provider.SetReClaude(false) })
	home, _ := os.UserHomeDir()
	reClaudeConfigDir := filepath.Join(home, ".rec", "claude", "accounts", "54787", "config")
	if err := os.MkdirAll(reClaudeConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".rec", "claude", "current"), []byte("54787\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := provider.SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	binary := claudeCLI{path: "/usr/local/bin/reclaude"}
	if provider.ReClaudeConfigDir() != reClaudeConfigDir {
		t.Fatalf("ReClaude's config folder is %q, want %q", provider.ReClaudeConfigDir(), reClaudeConfigDir)
	}
	if claudeTUIOn() {
		t.Fatal("the interactive mode is on with ReClaude on")
	}
	if code, _ := claudeMachineWhy(binary); code != "reclaude" {
		t.Fatalf("claudeMachineWhy says %q, want reclaude", code)
	}
	if tui, code := claudeTUI(&Request{}, binary); tui || code != "reclaude" {
		t.Fatalf("claudeTUI says %v %q with ReClaude's config folder known, want -p for reclaude", tui, code)
	}
	helperEnvironment := binary.tuiEnv(provider.WithReClaude(t.Context()), nil, "")
	if !slices.Contains(helperEnvironment, "CLAUDE_CONFIG_DIR="+reClaudeConfigDir) {
		t.Fatalf("the helper isn't told ReClaude's config folder: %v", helperEnvironment)
	}
	if ordinaryEnvironment := binary.tuiEnv(t.Context(), nil, ""); slices.ContainsFunc(ordinaryEnvironment, func(e string) bool { return strings.HasPrefix(e, "CLAUDE_CONFIG_DIR=") }) {
		t.Fatalf("a run not on ReClaude is told a config folder: %v", ordinaryEnvironment)
	}
}

func TestReClaudeWarmUpAsksSonnet(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	launcherDirectory := t.TempDir()
	modelFile := filepath.Join(launcherDirectory, "model")
	t.Setenv("FAKE_RECLAUDE_MODEL", modelFile)
	testenv.Program(t, filepath.Join(launcherDirectory, "reclaude"), "#!/bin/sh\nif [ \"$1\" = status ]; then printf 'daemon_running: true\\ndaemon_reachable: true\\n'; exit 0; fi\nprintf '%s' \"$5\" > \"$FAKE_RECLAUDE_MODEL\"\necho '{\"type\":\"result\",\"is_error\":false,\"result\":\"OK\"}'\n")
	t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := provider.SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.SetReClaude(false) })
	if err := warmClaude(provider.WithReClaude(t.Context()), ""); err != nil {
		t.Fatal(err)
	}
	if model, _ := os.ReadFile(modelFile); string(model) != "sonnet" {
		t.Fatalf("ReClaude keep-alive asked %q, want sonnet", model)
	}
}

// With ReClaude on, every Claude run is -p whatever the interactive mode's
// switch says (here with no ~/.rec, its config folder not known either):
// under the interactive mode (claudeTUIOn) a -p run is refused and never
// reused, so each of a conversation's turns would start a Claude Code
// anew. The gateway's tests
// run without the interactive mode's shim (no-shim), which hid that
// startRun refused a ReClaude run: the shim is at hand here, as it is in a
// real gateway, and a turn goes through.
func TestReClaudeRunsAsPrint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	fresh(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	was := claudeTUIRuns
	claudeTUIRuns = true
	t.Cleanup(func() { claudeTUIRuns = was })
	t.Cleanup(func() { _ = provider.SetReClaude(false) })
	binary := claudeCLI{path: "/usr/local/bin/reclaude"}
	if !claudeTUIOn() {
		t.Fatal("the interactive mode is off with ReClaude off")
	}
	if err := provider.SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	if claudeTUIOn() {
		t.Fatal("the interactive mode is on with ReClaude on")
	}
	if code, _ := claudeMachineWhy(binary); code != "reclaude" {
		t.Fatalf("claudeMachineWhy says %q, want reclaude", code)
	}
	if tui, code := claudeTUI(&Request{}, binary); tui || code != "reclaude" {
		t.Fatalf("claudeTUI says %v %q, want -p for reclaude", tui, code)
	}
	launcherDirectory := t.TempDir()
	testenv.Program(t, filepath.Join(launcherDirectory, "reclaude"), `#!/bin/sh
if [ "$1" = status ]; then printf 'daemon_running: true\ndaemon_reachable: true\n'; exit 0; fi
while IFS= read -r request; do
  echo '{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"claude-sonnet-5","usage":{"input_tokens":1}}}}'
  echo '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"from reclaude"}}}'
  echo '{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}}'
  echo '{"type":"stream_event","event":{"type":"message_stop"}}'
  echo '{"type":"result","subtype":"success","is_error":false,"result":"from reclaude"}'
done
`)
	t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	runsEndWithTest(t, launcherDirectory)
	server := New()
	t.Cleanup(server.subscription.abortAll)
	account := provider.Provider{ID: "claude", Account: &provider.Account{Agent: "claude", User: provider.ReClaudeUser, ReClaude: true}}
	body := `{"model":"claude-sonnet-5","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`
	recorder := httptest.NewRecorder()
	status, why := server.serveClaudeSubscription(recorder, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)), provider.Anthropic, account, "claude-sonnet-5", []byte(body), &Usage{})
	if status != 200 || !strings.Contains(recorder.Body.String(), "from reclaude") {
		t.Fatalf("a ReClaude turn with the interactive mode's shim at hand: %d %s: %s", status, why, recorder.Body.String())
	}
}
