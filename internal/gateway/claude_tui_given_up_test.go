package gateway

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/provider"
)

// An interactive run the client gives up on mid-reply can't be taken back
// and is ended; the turn sent again was told whole to a new Claude Code,
// the conversation written to the cache again (11-09: 60K tokens on each
// of two interrupts in a row). A copy of its session cut before the turn
// waits on the shelf, and the resend's run resumes it, told only the
// turn; a run started from that copy and given up on again leaves one too.
func TestClaudeInteractiveTurnGivenUpOnGoesOnFromACopyOfItsSession(t *testing.T) {
	h := newBusyRetryHarness(t, "interactive")
	logs := keepLog(t)
	// the first run resumed from a copy holds its turn, as the first
	// attempt's run does, so the client gives up on it too
	t.Setenv("FAKE_HOLD_RESUMED", filepath.Join(t.TempDir(), "held"))
	first := h.ask(`[` + rmsg("user", "the conversation so far, told at length. fix the bug") + `]`)
	if !strings.Contains(first, "turn 1") {
		t.Fatalf("the first turn: %q", first)
	}
	conv := `[` + rmsg("user", "the conversation so far, told at length. fix the bug") + `,` + rmsg("assistant", first) + `,` + rmsg("user", "and run the tests") + `]`
	giveUp := func(attempt int) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		body := h.body(conv)
		go func() {
			defer close(done)
			var u Usage
			request := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(body)).WithContext(ctx)
			h.s.serveClaudeSubscription(httptest.NewRecorder(), request, provider.Anthropic, h.p, "claude-sonnet-5", []byte(body), &u)
		}()
		deadline := time.Now().Add(10 * time.Second)
		for strings.Count(h.read(), "and run the tests") < attempt {
			if time.Now().After(deadline) {
				t.Fatalf("attempt %d was never told the turn:\n%s\nmagpie log:\n%s", attempt, h.read(), logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		<-done
		for strings.Count(logs.String(), "waits on the shelf as a copy of its session") < attempt {
			if time.Now().After(deadline) {
				t.Fatalf("attempt %d left no copy on the shelf:\n%s", attempt, logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	giveUp(1) // the run resumed for the turn
	giveUp(2) // the run started from its copy
	if again := h.ask(conv); !strings.Contains(again, "turn 1") {
		t.Fatalf("the turn sent a third time: %q\nmagpie log:\n%s", again, logs.String())
	}
	resumed := 0
	for name, told := range h.toldBy() {
		if !strings.Contains(told, "--resume") {
			continue
		}
		resumed++
		if strings.Contains(told, "the conversation so far") {
			t.Errorf("run %s was told the whole conversation:\n%.600s", name, told)
		}
		i := strings.Index(told, "--resume ")
		if sid := told[i+9:]; strings.HasPrefix(sid, "lead") {
			t.Errorf("run %s resumed a run's own session %.30s, want a copy", name, sid)
		}
	}
	if resumed != 2 {
		t.Fatalf("%d runs resumed a copy, want 2:\n%s", resumed, logs.String())
	}
}
