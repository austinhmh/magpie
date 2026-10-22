package provider

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	output, err := proc.ProbeContext(ctx, launcherPath, "status").Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("reclaude status: %w", ctx.Err())
		}
		return "", fmt.Errorf("reclaude status failed; run reclaude status to check the daemon: %w", err)
	}
	status := map[string]string{}
	for _, line := range bytes.Split(output, []byte{'\n'}) {
		name, value, found := bytes.Cut(line, []byte{':'})
		if found {
			status[strings.TrimSpace(string(name))] = strings.TrimSpace(string(value))
		}
	}
	if status["daemon_running"] != "true" || status["daemon_reachable"] != "true" {
		return "", errors.New("ReClaude daemon is not running and reachable; check reclaude status")
	}
	return launcherPath, nil
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
