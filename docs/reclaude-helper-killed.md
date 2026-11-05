# Troubleshooting: the Claude subscription loses its tools on a managed Mac

A runbook for one failure seen on the maintainer's managed macOS machine:
Claude-subscription (ReClaude) requests reach the model, but the model is
offered none of the client's tools, so it answers "I have no tools" or
writes its tool calls as plain text. The cause is local to that machine
(endpoint-security agents), not a bug in the bridge protocol. For the
design of the fix and why it is shaped this way, see the
[local ReClaude bridge record](reclaude-local-bridge.md); for the runtime
contract, the [Claude subscription bridge](subsystems/claude-subscription-bridge.md).

## Symptom

- A client (Cursor and others) gets replies, but the model has no tools:
  it says so, or its tool calls arrive as text.
- `serve.log` shows, per turn:

  ```
  claude: Claude Code's MCP connection to magpie is failed; this turn's request had none of the client's tools, so it is connected again
  claude: Claude Code did not connect magpie's MCP helper again: Claude settings: Connection closed
  claude: a reply wrote its tool calls as text ...; its run is told to make them
  ```

- Claude Code's own MCP log (under
  `~/Library/Caches/claude-cli-nodejs/.../mcp-logs-magpie/*.jsonl`) shows the
  helper closing before it answers:

  ```
  version negotiation probe closed the stdio server (rmcp-class pre-init hard close); respawning pinned legacy
  Connection failed after NNNms (CONNECTION_CLOSED): Connection closed
  ```

## How the helper fits in

With ReClaude on, every turn runs `claude -p`. Claude Code starts magpie's
own executable as `magpie claude-mcp-helper` (its MCP server), which passes
each tool call back to the gateway over HTTP. If that helper process cannot
start, Claude Code reports the MCP server failed and offers the model none
of the client's tools. The helper's executable path is
`claudeCLI.exe()` (`internal/gateway/claude_helper_exe.go`).

## Root cause

A process started from a **just-written or freshly replaced** executable is
killed on this machine before it runs. Two endpoint-security agents are
installed:

- CorpLink's system extension, holding
  `com.apple.developer.endpoint-security.client` (the kernel asks it on
  every exec).
- ThreatBook / Cardinal's process monitor.

The earlier restart script swapped a fresh copy of the binary into
`~/.local/bin` on **every** launch (`cp` then `mv`), so each launch handed
the agents a new, locally built, ad-hoc-signed 84 MB executable to judge.
Observed failure modes of a process started from such a file:

- launched by Claude Code: it exits within 50–260 ms without answering
  (the `Connection closed` above);
- launched from a Cursor terminal: it is left in uninterruptible exit
  (`ps` state `UE`), blocked on a turnstile owned by Cursor, that `kill -9`
  cannot reap. These accumulate until the machine is rebooted.

A byte-identical copy that is created **once and never rewritten** runs
normally (the `/tmp/mpnext` copy stayed up for hours). The two production
failure windows (10:56 and 11:10 on 2026-11-02) fell inside ThreatBook's
Cardinal rate-limit windows, when it disabled rules "through the driver"
under load; suggestive, not proven, and neither agent logged a disposition
against this file. Its security-side judgement logs are root-only
(`/Library/CorpLink/logs/av*.log`, `edlp*.log`; `/var/log/tbAgent/...`).

This is **not** confirmed to be a content/signature block. If it were,
only adding the binary to the agents' trust list would help. What is
confirmed is that a stable, created-once file is not killed, while a
rewritten-on-every-launch file is.

## The fix

Two parts, both aimed at "never hand the agents a freshly written
executable":

1. **Code** (`internal/gateway/claude_helper_exe.go`): the gateway copies
   its own executable once to `~/.cache/magpie/claude-helper/<content-hash>/magpie-reclaude`
   and runs the MCP helper from there (`selfCopy`, used by
   `claudeCLI.exe()`). The copy is reused unchanged across restarts, so its
   inode never changes. `os.Executable()` is the fallback; WSL is
   unaffected. Tests: `TestClaudeHelperExeCopiedOnce`,
   `TestClaudeHelperExeNewContentNewCopy`.
2. **Operation** (`~/magpie-scripts/start-magpie.sh`,
   `stop-magpie.sh`): the gateway binary lives at a fixed path
   `~/.magpie-run/magpie-reclaude`, installed once with `--install` and
   never swapped on a normal start. The launchd job points there.

The interactive-mode helper (`magpie claude-tui-helper`, `tuiCommand`)
still uses `os.Executable()`; it is not reached with ReClaude on (runs are
`claude -p`), so it is a follow-up, not part of this fix.

## Deploy (run in the macOS Terminal, not Cursor's)

Build a binary that carries the code change, install it once, and start:

```sh
cd ~/Desktop/dev/magpie-up
go build -tags nogui -o /tmp/magpie-reclaude .
zsh ~/magpie-scripts/start-magpie.sh --install /tmp/magpie-reclaude
```

Later restarts do not touch the binary:

```sh
zsh ~/magpie-scripts/start-magpie.sh
```

Script notes learned the hard way:

- The launchd job must be **reloaded** (`bootout` then `bootstrap`) after
  the program path or arguments change. `kickstart -k` only restarts the
  already-loaded definition and silently keeps the old path.
- Rebuild the whole `ProgramArguments` array, not
  `plutil -replace ProgramArguments.0`: replacing index 0 pushed the old
  path to index 1, where magpie read it as an agent name and exited with
  `unknown agent "/users/mi/.local/bin/magpie-reclaude"`.

## Verify it worked

```sh
# gateway up from the fixed path
launchctl print gui/$(id -u)/com.yetone.magpie.serve | grep -E '^\s*(state|program|pid) ='
# tools actually reach the model (expect stop_reason: tool_use)
curl -s http://127.0.0.1:3427/v1/messages -H 'Authorization: Bearer magpie' \
  -H 'content-type: application/json' -H 'anthropic-version: 2023-06-01' \
  -d '{"model":"claude/claude-sonnet-5-5","max_tokens":200,"messages":[{"role":"user","content":"Call echo_back with text ping. No prose."}],"tools":[{"name":"echo_back","description":"echo","input_schema":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}]}'
# the helper's stable copy exists and is reused
ls -la ~/.cache/magpie/claude-helper/*/
```

Verified on 2026-11-02: gateway running from `~/.magpie-run`, the request
returned `stop_reason: tool_use` calling `echo_back`, the cache copy was
created, and `serve.log` showed no MCP failure.

## If it comes back

The code/ops fix stops magpie from presenting a new executable each launch.
If helpers fail again anyway, treat it as a content/signature block by the
agents and take it to the machine's IT security, who can read the agents'
judgement logs and add the binary to the trust list. Evidence to hand them:

- the file path and its SHA-256, and that it is a locally built,
  ad-hoc-signed binary;
- the failure timestamps from `serve.log`;
- the stuck `UE` processes (`ps -axo pid,stat,command | grep magpie-reclaude`),
  which only a reboot clears;
- ThreatBook's rate-limit entries around the failures
  (`/var/log/tbAgent/AegisCore/*`, root-only).
