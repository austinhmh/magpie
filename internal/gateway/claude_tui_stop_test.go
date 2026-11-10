package gateway

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// giveUpOn sends conversation as a client that goes away once its run has
// been told turnWords the n-th time, and waits for the turn to be stopped
// the n-th time.
func giveUpOn(t *testing.T, harness *busyRetryHarness, logs interface{ String() string }, conversation, turnWords string, n int) {
	t.Helper()
	requestContext, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	body := harness.body(conversation)
	go func() {
		defer close(done)
		var usage Usage
		request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)).WithContext(requestContext)
		harness.s.serveClaudeSubscription(httptest.NewRecorder(), request, provider.Anthropic, harness.p, "claude-sonnet-5", []byte(body), &usage)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(harness.read(), turnWords) < n {
		if time.Now().After(deadline) {
			t.Fatalf("the run was never told %q:\n%s", turnWords, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	for strings.Count(logs.String(), "was stopped; its run waits") < n {
		if time.Now().After(deadline) {
			t.Fatalf("the turn wasn't stopped (%d):\n%s", n, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A run started for the turn, from a transcript of the conversation
// (replaySession), that the client gives up on is stopped and kept: the
// resend goes on in the same Claude Code, told only its own message (11-09
// 17:50, where each interrupt had a new Claude Code told the conversation
// again).
func TestClaudeInteractiveTurnGivenUpOnIsStoppedAndItsRunKept(t *testing.T) {
	t.Setenv("FAKE_HOLD_ESC", "1")
	harness := newBusyRetryHarness(t, "interactive")
	logs := keepLog(t)
	history := rmsg("user", "the conversation so far, told at length. fix the bug") + `,` + rmsg("assistant", "fixed it")
	giveUpOn(t, harness, logs, `[`+history+`,`+rmsg("user", "and run the tests")+`]`, "and run the tests", 1)
	again := harness.ask(`[` + history + `,` + rmsg("user", "and run the tests, please") + `]`)
	if !strings.Contains(again, "turn 2") {
		t.Fatalf("the resend: %q\n%s", again, logs.String())
	}
	told := harness.toldBy()
	if len(told) != 1 {
		t.Fatalf("%d Claude Codes ran, want the one kept:\n%s", len(told), logs.String())
	}
	for name, said := range told {
		// told the turn given up on, then the resend's message alone
		var lines []string
		for _, line := range strings.Split(said, "\n") {
			if strings.HasPrefix(line, "told ") {
				lines = append(lines, line)
			}
		}
		if len(lines) != 2 {
			t.Fatalf("run %s was told %d messages, want the turn and its resend:\n%s", name, len(lines), said)
		}
		if resend := lines[1]; !strings.Contains(resend, "and run the tests, please") || strings.Contains(resend, "fixed it") || strings.Contains(resend, "the conversation so far") {
			t.Errorf("the resend was told more than its own message:\n%s", resend)
		}
	}
}

// Two interrupts in a row, as 15:56 had: the kept run is stopped again,
// and the third send still goes on in it.
func TestClaudeInteractiveTurnGivenUpOnTwiceKeepsItsRun(t *testing.T) {
	t.Setenv("FAKE_HOLD_ESC", "1")
	t.Setenv("FAKE_HOLD_ESC_AGAIN", "2")
	harness := newBusyRetryHarness(t, "interactive")
	logs := keepLog(t)
	history := rmsg("user", "the conversation so far, told at length. fix the bug") + `,` + rmsg("assistant", "fixed it")
	conversation := `[` + history + `,` + rmsg("user", "and run the tests") + `]`
	giveUpOn(t, harness, logs, conversation, "and run the tests", 1)
	giveUpOn(t, harness, logs, conversation, "and run the tests", 2)
	if again := harness.ask(conversation); !strings.Contains(again, "turn 3") {
		t.Fatalf("the third send: %q\n%s", again, logs.String())
	}
	if told := harness.toldBy(); len(told) != 1 {
		t.Fatalf("%d Claude Codes ran, want the one kept:\n%s", len(told), logs.String())
	}
}

// A call still in the client's hands when it gives up on the turn is
// answered as interrupted, and the run waits at the conversation before
// the turn.
func TestClaudeStoppedTurnAnswersItsPendingCall(t *testing.T) {
	bridge := New().subscription
	waiter := make(chan mcpToolResult, 1)
	run := &subscriptionRun{bridge: bridge, tui: true, backKey: "before", timer: time.AfterFunc(time.Hour, func() {}),
		pending: map[string]chan mcpToolResult{"call_1": waiter}}
	t.Cleanup(func() { run.timer.Stop() })
	run.stdin = &reconnectAnswering{run: run}
	bridge.calls["call_1"] = run
	run.letGo()
	select {
	case got := <-waiter:
		if !got.IsError {
			t.Fatalf("the pending call was answered %+v", got)
		}
	default:
		t.Fatal("the pending call was left waiting")
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.idle["before"] != run || bridge.calls["call_1"] != nil {
		t.Fatalf("kept at the turn before: %v, call still routed to it: %v", bridge.idle["before"] == run, bridge.calls["call_1"] != nil)
	}
}

// A run waiting on a call the client dropped, its user having stopped the
// tool while it ran, goes on with the client's next message: that message
// follows the conversation before the turn and carries no call of it
// (11-10 13:44, where it found the run "busy" and a new Claude Code was
// told the whole conversation again). A request that still carries the
// call, or offers no tools, leaves the run waiting on it.
func TestClaudeTurnWhoseCallsTheClientDroppedGoesOnInItsRun(t *testing.T) {
	owner := "claude\x00u"
	first := Message{Role: "user", Parts: []Part{{Kind: Text, Text: "the conversation so far. fix the bug"}}}
	reply := Message{Role: "assistant", Parts: []Part{{Kind: Text, Text: "fixed it"}}}
	asked := Message{Role: "user", Parts: []Part{{Kind: Text, Text: "uppercase every file"}}}
	called := Message{Role: "assistant", Parts: []Part{{Kind: ToolCall, ID: "call_d", Name: "Write"}}}
	next := Message{Role: "user", Parts: []Part{{Kind: Text, Text: "go on with what was stopped"}}}
	conversation := func(since ...Message) []Message { return append([]Message{first, reply}, since...) }
	cases := []struct {
		name    string
		req     *Request
		stopped bool
	}{
		{"the call left out", &Request{Tools: []Tool{{Name: "Write"}}, Messages: conversation(next)}, true},
		{"the call still carried", &Request{Tools: []Tool{{Name: "Write"}}, Messages: conversation(asked, called, next)}, false},
		{"an aside with no tools", &Request{Messages: conversation(next)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bridge := New().subscription
			key, _ := nextTurn(c.req, owner)
			waiter := make(chan mcpToolResult, 1)
			run := &subscriptionRun{bridge: bridge, tui: true, backKey: key, parkedAt: time.Now(),
				tools: map[string]bool{"Write": true}, timer: time.AfterFunc(time.Hour, func() {}),
				pending: map[string]chan mcpToolResult{"call_d": waiter}}
			t.Cleanup(func() { run.timer.Stop() })
			run.owner.Store(&owner)
			run.stdin = &reconnectAnswering{run: run}
			bridge.runs["run"] = run
			bridge.calls["call_d"] = run
			got, _ := bridge.resume(c.req, owner)
			if (got == run) != c.stopped {
				t.Fatalf("the next message went on in the waiting run: %v, want %v", got == run, c.stopped)
			}
			select {
			case answer := <-waiter:
				if !c.stopped || !answer.IsError {
					t.Fatalf("the call was answered %+v", answer)
				}
			default:
				if c.stopped {
					t.Fatal("the dropped call was left waiting")
				}
			}
		})
	}
}
