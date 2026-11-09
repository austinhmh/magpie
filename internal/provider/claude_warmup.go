package provider

// Starting a Claude account's next window as soon as the last one resets,
// as codex_warmup.go does a ChatGPT account's: a Claude window, the 5-hour
// one and the weekly one, starts at the account's first request after it
// resets. The request goes through Claude Code itself, as the gateway's
// Claude Subscription does, so it is Claude Code's own; the gateway, which
// runs Claude Code, says how.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

func claudeWarmPath() string { return filepath.Join(filepath.Dir(Path()), "claude-warmup.json") }

// claudeWarmUsage is each Claude account's windows, the cached reads the
// Usage page shares; an account whose sign-in lapsed is left out.
func claudeWarmUsage(ctx context.Context) map[string]SubscriptionQuota {
	u := LoginUsage(ctx, "claude")
	for _, l := range Logins("claude") {
		if l.Lapsed != "" {
			delete(u, l.User)
		}
	}
	return u
}

// warmClaudeLogin is a warm-up's send for the Claude accounts: send asks
// Claude Code, with configDir the config directory of a saved account
// (claude_dirs.go), "" for the one Claude Code is signed in to.
func warmClaudeLogin(send func(ctx context.Context, configDir string) error) func(context.Context, string) error {
	return func(ctx context.Context, user string) error {
		if selected, err := ReClaudeEnabled(); err != nil || selected {
			return fmt.Errorf("Claude Code warm-up is unavailable while the account source is ReClaude or unknown")
		}
		// the account's own proxy, which send's Claude Code runs with too
		ctx = ViaLogin(ctx, "claude", user)
		ls := Logins("claude")
		i := slices.IndexFunc(ls, func(l Login) bool { return strings.EqualFold(l.User, user) })
		if i < 0 {
			return fmt.Errorf("no Claude account %q", user)
		}
		if !ls[i].Active {
			return inSavedClaudeDir(ls[i].User, func(dir string) error { return send(ctx, dir) })
		}
		return send(ctx, "")
	}
}

// claudeWindowsExpected is the windows every Claude account has: Anthropic
// leaves them out (null) for an account never used.
var claudeWindowsExpected = []QuotaWindow{{Name: "5 hours", Span: 5 * time.Hour}, {Name: "7 days", Span: 7 * 24 * time.Hour}}

// claudeWarmedNow has what the Claude account user has left read again,
// not taken from the last few minutes' read: the window a warm-up started
// has its reset known, and routing ranks the account by it at once. The
// next read is due as for an account never read (claudeWindows), and what
// is kept keeps its own time until then: cleared, the card stamped the
// windows read before the warm-up as read just now, and no read was due
// (koohoko5924).
func claudeWarmedNow(user string) {
	key := strings.ToLower(user)
	c := &claudeUsage
	c.Lock()
	if e, ok := c.m[key]; ok {
		e.tried = time.Time{}
		c.m[key] = e
	}
	c.Unlock()
	StaleAllowance("claude", user)
}

// ClaudeWarmed is when magpie last started a window of each Claude account,
// by user as kept (lower-cased).
func ClaudeWarmed() map[string]time.Time { return codexWarmedIn(claudeWarmPath()) }

// KeepClaudeWindowsWarm starts the Claude accounts' windows as they reset,
// while settings say to, each with one request send makes.
func KeepClaudeWindowsWarm(ctx context.Context, send func(ctx context.Context, configDir string) error) {
	go keepReClaudeAlive(ctx, send) // runs alongside; it does nothing while ReClaude is off
	if selected, _ := ReClaudeEnabled(); selected {
		return // the personal accounts' warm-up never runs on the synthetic source
	}
	w := codexWarmer{path: claudeWarmPath(), now: time.Now, usage: claudeWarmUsage, send: warmClaudeLogin(send),
		expect: claudeWindowsExpected, warmed: claudeWarmedNow}
	keepWarm(ctx, "claude", w, func() (string, []string) { s := settings.Load(); return s.ClaudeWarmup, s.ClaudeWarmAts })
}

// reClaudeKeepAliveWindow is the entry the ReClaude keep-alive keeps under
// the synthetic user in claude-warmup.json, beside the accounts' windows.
const reClaudeKeepAliveWindow = "keep-alive"

// reClaudeKeepAliveRetry is how long a failed keep-alive waits before the
// next try, while its time of day is still within dailySlack.
const reClaudeKeepAliveRetry = 5 * time.Minute

// reClaudeKeepAliveDue says whether the latest of the times of day ats has
// passed by no more than dailySlack and the keep-alive (last sent for the
// start last) hasn't had it; key is the start to keep once it is sent.
func reClaudeKeepAliveDue(ats []string, last string, now time.Time) (key string, due bool) {
	start, ok := dayStart(ats, now)
	if !ok || now.Sub(start) > dailySlack {
		return "", false
	}
	key = startKey(start)
	return key, !hadStart(last, key)
}

// reClaudeKeepAliveNow sends the ReClaude source one message through send
// when one of ats is due, whether or not its 5-hour window is running, and
// keeps the start it was sent for in path; sent says whether it tried.
func reClaudeKeepAliveNow(ctx context.Context, path string, ats []string, now time.Time, send func(context.Context, string) error) (sent bool, err error) {
	state := readWarmState(path)
	userKey := strings.ToLower(ReClaudeUser)
	key, due := reClaudeKeepAliveDue(ats, state[userKey][reClaudeKeepAliveWindow].Daily, now)
	if !due {
		return false, nil
	}
	if err := send(WithReClaude(ctx), ""); err != nil {
		return true, err // nothing kept: tried again while the start is due
	}
	if state[userKey] == nil {
		state[userKey] = map[string]warmWindow{}
	}
	state[userKey][reClaudeKeepAliveWindow] = warmWindow{Warmed: now, Daily: key}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return true, err
	}
	return true, writePrivate(path, append(body, '\n'))
}

// startingReClaude is send for the keep-alive: a daemon `reclaude login` or
// `reclaude stop` left stopped is started first (StartReClaudeDaemon); one
// running is left as it is. Only the keep-alive starts it: a request's own
// check (ReClaudeBinary) refuses as before.
func startingReClaude(send func(ctx context.Context, configDir string) error) func(context.Context, string) error {
	return func(ctx context.Context, configDir string) error {
		if _, err := ReClaudeBinary(ctx); errors.Is(err, errReClaudeDaemonDown) {
			log.Printf("claude warm-up: ReClaude daemon is not running; starting it")
			if _, err := StartReClaudeDaemon(ctx); err != nil {
				return err
			}
		}
		return send(ctx, configDir)
	}
}

// keepReClaudeAlive looks every minute, until ctx ends, and sends the
// keep-alive at each of settings' ClaudeWarmAts while the source is
// ReClaude, discovery is on and the claude provider isn't left alone.
func keepReClaudeAlive(ctx context.Context, send func(context.Context, string) error) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	sendStarted := startingReClaude(send)
	var retryAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		now := time.Now().Round(0) // the wall clock, which goes on while the machine sleeps
		if now.Before(retryAt) {
			continue
		}
		if selected, err := ReClaudeEnabled(); err != nil || !selected || discoveryOff() || leftAlone("claude") {
			continue
		}
		sendContext, cancel := context.WithTimeout(ctx, 3*time.Minute) // the daemon's start, Claude Code's, Sonnet's answer
		sent, err := reClaudeKeepAliveNow(sendContext, claudeWarmPath(), settings.Load().ClaudeWarmAts, now, sendStarted)
		cancel()
		switch {
		case err != nil:
			retryAt = now.Add(reClaudeKeepAliveRetry)
			log.Printf("claude warm-up: ReClaude keep-alive: %v", err)
		case sent:
			claudeWarmedNow(ReClaudeUser) // its allowance read again, so the Usage card shows the window
			log.Printf("claude warm-up: sent ReClaude its keep-alive")
		}
	}
}
