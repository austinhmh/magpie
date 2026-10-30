package gateway

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// mcpDownInit is Claude Code 2.1.310's init for a turn whose MCP connection
// to magpie's helper closed during the server/discover probe (10-31 01:01:
// "rmcp-class pre-init hard close"): its request then went out with no
// tools at all.
const mcpDownInit = `{"type":"system","subtype":"init","session_id":"s-down","tools":[],"mcp_servers":[{"name":"magpie","status":"failed","source":"dynamic"}]}`

// mcpUpInit is the same once the helper is connected.
const mcpUpInit = `{"type":"system","subtype":"init","session_id":"s-up","tools":["mcp__magpie__Shell"],"mcp_servers":[{"name":"magpie","status":"connected","source":"dynamic"}]}`

// reconnectAnswering stands in for Claude Code's input: it records each line
// and answers a control request (mcp_reconnect) on the run, as Claude Code
// answers it on its output.
type reconnectAnswering struct {
	linesTold
	run       *subscriptionRun
	mu        sync.Mutex
	asked     []string
	answerLag time.Duration
	answered  bool // Claude Code has answered a reconnect
	early     int  // user lines told before it had
}

func (w *reconnectAnswering) Write(b []byte) (int, error) {
	var ctl struct {
		Type      string         `json:"type"`
		RequestID string         `json:"request_id"`
		Request   map[string]any `json:"request"`
	}
	if json.Unmarshal(b, &ctl) == nil && ctl.Type == "control_request" {
		w.mu.Lock()
		server, _ := ctl.Request["serverName"].(string)
		w.asked = append(w.asked, ctl.Request["subtype"].(string)+":"+server)
		w.mu.Unlock()
		go func() {
			time.Sleep(w.answerLag)
			w.mu.Lock()
			w.answered = true
			w.mu.Unlock()
			w.run.answered(json.RawMessage(`{"subtype":"success","request_id":"` + ctl.RequestID + `"}`))
		}()
		return len(b), nil
	}
	w.mu.Lock()
	if !w.answered {
		w.early++
	}
	w.mu.Unlock()
	return w.linesTold.Write(b)
}

func (w *reconnectAnswering) controls() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.asked...)
}

// A turn whose request went out without magpie's tools (Claude Code's MCP
// connection to the helper failed: its init says so) has the model write
// its calls as markup with the client's own name, `<invoke name="Shell">`.
// None of it reaches the client: Claude Code is asked to connect the helper
// again, and only once it has is the run told to make the calls, which it
// then does for real. Before, Cursor showed the commands as the reply.
func TestClaudeCallsWrittenWithoutToolsAreMadeOnceMCPIsBack(t *testing.T) {
	run := &subscriptionRun{}
	in := &reconnectAnswering{run: run, answerLag: 200 * time.Millisecond}
	run.stdin = in
	lines := append([]string{mcpDownInit}, textCallTurn("msg_bare",
		"我先查一下 DNS 解析和常用端口。\n\n\n",
		`<invoke name="Shell">`+"\n",
		`<parameter name="command">dig +short A example.com</parameter>`+"\n",
		`<parameter name="description">检查 DNS 解析与常用端口连通性</parameter>`+"\n</invoke>")...)
	lines = append(lines, mcpUpInit)
	lines = append(lines, realCallTurn...)
	text, stop, _, _, _, _, raw, ended, ok := claudeReply(t, run, lines, true)
	if strings.Contains(raw, "<invoke") || strings.Contains(raw, "<parameter") || strings.Contains(raw, "检查 DNS") {
		t.Fatalf("the client was shown the calls written without tools:\n%s", raw)
	}
	if text != "我先查一下 DNS 解析和常用端口。\n\n\n[mcp__ccd_turn__update_plan]" || stop != "tool_use" || ended != "tool" || !ok {
		t.Fatalf("reply %q stop %q, run ended %q %v:\n%s", text, stop, ended, ok, raw)
	}
	// the ask goes once Claude Code has answered the reconnect, off the reader
	var asks []string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if asks = in.lines(); len(asks) > 0 {
			break
		}
	}
	if len(asks) != 1 || !strings.Contains(asks[0], "Make the calls with your tools now") {
		t.Fatalf("the run was told %q, want the one ask to make the calls", asks)
	}
	if got := in.controls(); len(got) != 1 || got[0] != "mcp_reconnect:magpie" {
		t.Fatalf("Claude Code was asked %q, want one mcp_reconnect of magpie", got)
	}
	// told before the helper was back, its next request had no tools either
	in.mu.Lock()
	early := in.early
	in.mu.Unlock()
	if early != 0 {
		t.Fatalf("the run was told to make its calls before Claude Code had connected magpie's helper again")
	}
}

// With magpie's helper connected, Claude Code is asked nothing of it: the
// reply is told as it comes.
func TestClaudeCallsWrittenWithoutToolsNoReconnectWhenUp(t *testing.T) {
	run := &subscriptionRun{}
	in := &reconnectAnswering{run: run}
	run.stdin = in
	lines := append([]string{mcpUpInit}, textCallTurn("msg_up", "All good.\n")...)
	text, stop, _, _, _, _, raw, _, _ := claudeReply(t, run, lines, true)
	if text != "All good.\n" || stop != "end_turn" {
		t.Fatalf("reply %q stop %q:\n%s", text, stop, raw)
	}
	if got := in.controls(); len(got) != 0 {
		t.Fatalf("Claude Code was asked %q with its helper connected", got)
	}
}

// Offered no tools, a model wrote the same call as text over and over and
// never ended its turn (60 KB in two minutes, until the client gave up). Past
// textCallsMost magpie interrupts it; Claude Code then ends the message with
// no stop reason and an error result (aborted_streaming). The reply goes on
// as one that wrote its calls as text: the client is shown none of it and
// gets the real call.
func TestClaudeRunawayTextCallsAreStopped(t *testing.T) {
	run := &subscriptionRun{}
	in := &reconnectAnswering{run: run}
	run.stdin = in
	call := `<invoke name="Bash">` + "\n" + `<parameter name="command">dig +short example.com</parameter>` + "\n</invoke>\n\n"
	chunks := []string{"Checking.\n"}
	for i := 0; i*len(call) < 2*textCallsMost; i++ {
		chunks = append(chunks, call)
	}
	lines := []string{mcpDownInit,
		`{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_loop","model":"claude-sonnet-5-5","usage":{"input_tokens":3,"output_tokens":1}}}}`,
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}`,
	}
	for _, c := range chunks {
		b, _ := json.Marshal(c)
		lines = append(lines, `{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(b)+`}}}`)
	}
	// interrupted: the block and message stop, with no message_delta, and
	// the turn's result is an error (aborted_streaming), which ends nothing
	lines = append(lines,
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0}}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
		`{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming"}`,
		mcpUpInit)
	lines = append(lines, realCallTurn...)
	text, stop, _, _, _, _, raw, ended, ok := claudeReply(t, run, lines, true)
	if strings.Contains(raw, "<invoke") || strings.Contains(raw, "dig +short") {
		t.Fatalf("the client was shown the calls written as text:\n%.600s", raw)
	}
	if text != "Checking.\n[mcp__ccd_turn__update_plan]" || stop != "tool_use" || ended != "tool" || !ok {
		t.Fatalf("reply %q stop %q, run ended %q %v:\n%.600s", text, stop, ended, ok, raw)
	}
	var asks []string
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if asks = in.lines(); len(asks) > 0 {
			break
		}
	}
	got := in.controls()
	if !slices.Contains(got, "interrupt:") || !slices.Contains(got, "mcp_reconnect:magpie") {
		t.Fatalf("Claude Code was asked %q, want an interrupt and a reconnect", got)
	}
	if len(asks) != 1 || !strings.Contains(asks[0], "Make the calls with your tools now") {
		t.Fatalf("the run was told %q, want the one ask to make the calls", asks)
	}
}
