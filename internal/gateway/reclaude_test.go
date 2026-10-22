package gateway

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
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
				owner:   "claude\x00" + provider.ReClaudeUser + "\x00reclaude",
				pending: map[string]chan mcpToolResult{"echo-call": pendingResult},
			}
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
