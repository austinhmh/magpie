package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/testenv"
)

// The ReClaude keep-alive goes at each time of day whether or not its
// window runs, once per time, with the source on its context; a failed
// one is not kept, so it goes again.
func TestReClaudeKeepAliveSendsAtEachTimeOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-warmup.json")
	ats := []string{"07:00", "12:00", "17:00", "22:00"}
	day := func(hour, minute int) time.Time { return time.Date(2026, 10, 26, hour, minute, 0, 0, time.Local) }
	var sends []bool // each send's context says ReClaude
	var fail error
	send := func(ctx context.Context, configDir string) error {
		sends = append(sends, ReClaudeFrom(ctx) && configDir == "")
		return fail
	}
	for _, step := range []struct {
		at       time.Time
		wantSent bool
	}{
		{day(6, 59), false},  // before the first time
		{day(7, 0), true},    // the first time
		{day(7, 30), false},  // already sent for 07:00
		{day(12, 5), true},   // the next time, whatever the window
		{day(13, 30), false}, // 12:00 had; 17:00 not yet
		{day(18, 30), false}, // 17:00 passed by more than dailySlack
		{day(22, 0), true},
	} {
		sent, err := reClaudeKeepAliveNow(context.Background(), path, ats, step.at, send)
		if err != nil || sent != step.wantSent {
			t.Fatalf("at %s: sent=%v err=%v, want sent=%v", step.at.Format("15:04"), sent, err, step.wantSent)
		}
	}
	for i, viaReClaude := range sends {
		if !viaReClaude {
			t.Errorf("send %d did not carry the ReClaude source", i)
		}
	}
	retryPath := filepath.Join(t.TempDir(), "retry.json")
	fail = errors.New("daemon down")
	if sent, err := reClaudeKeepAliveNow(context.Background(), retryPath, ats, day(7, 0), send); !sent || err == nil {
		t.Fatalf("failed send: sent=%v err=%v", sent, err)
	}
	fail = nil
	if sent, err := reClaudeKeepAliveNow(context.Background(), retryPath, ats, day(7, 5), send); !sent || err != nil {
		t.Fatalf("a failed keep-alive must go again: sent=%v err=%v", sent, err)
	}
}

// A daemon `reclaude login` left stopped is started before the keep-alive's
// send, once; one running is left alone. The send checks the daemon as the
// real one does (claudeBinaryFor), so without the start it fails.
func TestReClaudeKeepAliveStartsAStoppedDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake launcher is a shell script")
	}
	limitsHome(t)
	t.Setenv("MAGPIE_NO_ACCOUNTS", "")
	launcherDirectory := t.TempDir()
	startsFile := filepath.Join(launcherDirectory, "starts")
	t.Setenv("FAKE_RECLAUDE_STARTS", startsFile)
	testenv.Program(t, filepath.Join(launcherDirectory, "reclaude"), `#!/bin/sh
if [ "$1" = status ]; then
  if [ -s "$FAKE_RECLAUDE_STARTS" ]; then printf 'daemon_running: true\ndaemon_reachable: true\n'; else printf 'daemon_running: false\ndaemon_reachable: false\n'; fi
  exit 0
fi
if [ "$1" = daemon ] && [ "$2" = --detach ]; then echo started >> "$FAKE_RECLAUDE_STARTS"; exit 0; fi
exit 9
`)
	t.Setenv("PATH", launcherDirectory+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := SetReClaude(true); err != nil {
		t.Fatal(err)
	}
	sent := 0
	send := startingReClaude(func(ctx context.Context, configDir string) error {
		if _, err := ReClaudeBinary(ctx); err != nil {
			return err
		}
		sent++
		return nil
	})
	for round := 1; round <= 2; round++ {
		if err := send(WithReClaude(context.Background()), ""); err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
	}
	starts, _ := os.ReadFile(startsFile)
	if started := strings.Count(string(starts), "started"); sent != 2 || started != 1 {
		t.Fatalf("sent %d, daemon started %d times; want 2 and 1", sent, started)
	}
}
