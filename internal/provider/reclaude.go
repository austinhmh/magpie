package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yetone/magpie/internal/appdir"
	"github.com/yetone/magpie/internal/proc"
	"github.com/yetone/magpie/internal/settings"
)

// ReClaudeUser is the synthetic account's stable identity in quota and
// affinity records. It is not a login stored in logins.json.
const ReClaudeUser = "ReClaude"

const reClaudeUser = ReClaudeUser

// ReClaudeEnabled refuses to infer a personal Claude account from unreadable
// settings: the source remains unknown until the file can be read again.
func ReClaudeEnabled() (bool, error) {
	selected, err := settings.LoadChecked()
	if err != nil {
		return false, err
	}
	return selected.ReClaude, nil
}

// SetReClaude switches the account source without touching either login.
func SetReClaude(on bool) error {
	if err := settings.SetReClaude(on); err != nil {
		return err
	}
	ForgetAccounts()
	return nil
}

// ReClaudeState is deliberately limited to operational status; it never
// contains the launcher configuration, cookies, tokens or certificate.
type ReClaudeState struct {
	On    bool   `json:"on"`
	Ready bool   `json:"ready"`
	Why   string `json:"why,omitempty"`
}

func ReClaudeStatus() ReClaudeState {
	on, err := ReClaudeEnabled()
	state := ReClaudeState{On: on}
	if err == nil && on {
		_, err = ReClaudeBinary(context.Background())
		state.Ready = err == nil
	}
	if err != nil {
		state.Why = err.Error()
	}
	return state
}

// ReClaudeBinary checks the selected source and its daemon, returning the
// same launcher it checked. A caller's cancellation also ends the probe.
func ReClaudeBinary(ctx context.Context) (string, error) {
	on, err := ReClaudeEnabled()
	if err != nil {
		return "", err
	}
	if !on {
		return "", errors.New("ReClaude is off")
	}
	if discoveryOff() {
		return "", errAccountDiscoveryOff
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	launcherPath := proc.FindTool("reclaude")
	if launcherPath == "" {
		return "", errors.New("reclaude is not on PATH; install it and run reclaude login")
	}
	output, err := proc.ProbeContext(ctx, launcherPath, "status", "--check", "--json").Output()
	if ctx.Err() != nil {
		return "", fmt.Errorf("reclaude status: %w", ctx.Err())
	}
	// Parsed apart from the health check, so a field of another type here
	// never changes what the check decides. An older launcher's text parses
	// to nothing.
	var located struct {
		Global struct {
			DataDir string `json:"data_dir"`
		} `json:"global"`
		Claude struct {
			Current struct {
				DeviceID json.Number `json:"device_id"`
			} `json:"current"`
		} `json:"claude"`
	}
	_ = json.Unmarshal(output, &located)
	statusConfigDir := ""
	if located.Global.DataDir != "" && located.Claude.Current.DeviceID != "" {
		statusConfigDir = filepath.Join(located.Global.DataDir, "claude", "accounts", located.Claude.Current.DeviceID.String(), "config")
	}
	reClaudeStatusConfigDir.Store(&statusConfigDir)
	var checkedStatus struct {
		Checked bool `json:"checked"`
		Global  struct {
			Daemon struct {
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			} `json:"daemon"`
		} `json:"global"`
	}
	if json.Unmarshal(output, &checkedStatus) == nil {
		if !checkedStatus.Checked {
			return "", errors.New("reclaude status did not verify the daemon; run reclaude status --check")
		}
		daemonState := checkedStatus.Global.Daemon.Status.State
		if daemonState == "" || daemonState == "unverified" {
			return "", errors.New("reclaude status did not report verified daemon health; run reclaude status --check")
		}
		if daemonState != "reachable" {
			return "", errReClaudeDaemonDown
		}
		if err != nil {
			return "", fmt.Errorf("reclaude status --check --json failed: %w", err)
		}
		return launcherPath, nil
	}
	if err != nil {
		// Older launchers do not accept the checked JSON status flags.
		output, err = proc.ProbeContext(ctx, launcherPath, "status").Output()
		if err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("reclaude status: %w", ctx.Err())
			}
			return "", fmt.Errorf("reclaude status failed; run reclaude status to check the daemon: %w", err)
		}
	}
	status := map[string]string{}
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		name, value, found := bytes.Cut(line, []byte{':'})
		if found {
			status[strings.TrimSpace(string(name))] = strings.TrimSpace(string(value))
		}
	}
	if _, reported := status["daemon_running"]; !reported {
		return "", errors.New("reclaude status did not report daemon health; run reclaude status --check")
	}
	if status["daemon_running"] != "true" || status["daemon_reachable"] != "true" {
		return "", errReClaudeDaemonDown // the keep-alive tells this apart (startingReClaude)
	}
	return launcherPath, nil
}

// reClaudeStatusConfigDir is the config folder the last `reclaude status
// --check --json` named (data_dir and claude.current.device_id), "" when
// it named none. Every run checks the status first (ReClaudeBinary), and
// each read replaces it, so an account switched since isn't kept.
var reClaudeStatusConfigDir atomic.Pointer[string]

// ReClaudeConfigDir is the Claude Code config folder ReClaude's launcher
// runs Claude Code in: <data_dir>/claude/accounts/<device_id>/config as the
// launcher's status names them, else for the account ~/.rec/claude/current
// names. A ReClaude run has magpie's own CLAUDE_CONFIG_DIR taken out
// (cleanReClaudeEnv), so its sessions are kept there, not in ~/.claude.
// "" when neither names a folder that is there: unknown, not ~/.claude.
func ReClaudeConfigDir() string {
	if fromStatus := reClaudeStatusConfigDir.Load(); fromStatus != nil && *fromStatus != "" {
		if info, err := os.Stat(*fromStatus); err == nil && info.IsDir() {
			return *fromStatus
		}
	}
	home, err := appdir.Home()
	if err != nil {
		return ""
	}
	reClaudeRoot := filepath.Join(home, ".rec", "claude")
	currentAccount, err := os.ReadFile(filepath.Join(reClaudeRoot, "current"))
	if err != nil {
		return ""
	}
	accountID := strings.TrimSpace(string(currentAccount))
	if accountID == "" || accountID == "." || accountID == ".." || accountID != filepath.Base(accountID) {
		return ""
	}
	configDir := filepath.Join(reClaudeRoot, "accounts", accountID, "config")
	if info, err := os.Stat(configDir); err != nil || !info.IsDir() {
		return ""
	}
	return configDir
}

// errReClaudeDaemonDown is ReClaudeBinary's answer for a launcher found with
// its daemon stopped or unreachable (`reclaude login` and `reclaude stop`
// leave it so, and launchd's KeepAlive doesn't bring it back). The
// keep-alive starts the daemon on it; a request never does.
var errReClaudeDaemonDown = errors.New("ReClaude daemon is not running and reachable; check reclaude status")

// reClaudeDaemonStart is how long StartReClaudeDaemon waits for the daemon
// to answer after `reclaude daemon --detach`: launchctl brought it up in
// 13 seconds on 10-25.
const reClaudeDaemonStart = 30 * time.Second

// StartReClaudeDaemon runs `reclaude daemon --detach`, which hands the
// daemon to launchd (systemd on Linux) and returns, then waits up to
// reClaudeDaemonStart for ReClaudeBinary to find it running, returning the
// launcher as ReClaudeBinary does. It starts the machine's one daemon (the
// launchd service com.reclaude.daemon, shared with the user's own Claude
// Code), never an instance of magpie's own. proc.CommandContext, not
// ProbeContext: a probe's whole group is killed when its context ends, and
// the daemon must outlive the command that started it.
func StartReClaudeDaemon(ctx context.Context) (string, error) {
	launcherPath := proc.FindTool("reclaude")
	if launcherPath == "" {
		return "", errors.New("reclaude is not on PATH; install it and run reclaude login")
	}
	ctx, cancel := context.WithTimeout(ctx, reClaudeDaemonStart)
	defer cancel()
	if out, err := proc.CommandContext(ctx, launcherPath, "daemon", "--detach").CombinedOutput(); err != nil {
		return "", fmt.Errorf("reclaude daemon --detach: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for {
		path, err := ReClaudeBinary(ctx)
		if !errors.Is(err, errReClaudeDaemonDown) {
			return path, err // running, or an error that waiting won't mend
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("ReClaude daemon not reachable %s after reclaude daemon --detach", reClaudeDaemonStart)
		case <-time.After(2 * time.Second):
		}
	}
}

func reClaudeAccount() Provider {
	account := &Account{Agent: "claude", User: reClaudeUser, Plan: "ReClaude", ReClaude: true}
	result := claudeProvider(account)
	result.Name = "ReClaude"
	return result
}

func reClaudeLogin() Login {
	return Login{Agent: "claude", User: reClaudeUser, Plan: "ReClaude", Active: true, On: true}
}

type reClaudeContextKey struct{}

// WithReClaude keeps the source on a Claude Code subprocess call, not in
// shared process state (model probes and allowance reads run concurrently).
func WithReClaude(ctx context.Context) context.Context {
	return context.WithValue(ctx, reClaudeContextKey{}, true)
}

func ReClaudeFrom(ctx context.Context) bool {
	selected, _ := ctx.Value(reClaudeContextKey{}).(bool)
	return selected
}

// personalClaudeAvailable refuses sign-in changes and stale personal
// accounts while the other source is selected or its setting is unknown.
func personalClaudeAvailable() error {
	selected, err := ReClaudeEnabled()
	if err != nil {
		return err
	}
	if selected {
		return errors.New("Claude Code accounts are not used while ReClaude is on; turn it off first")
	}
	return nil
}
