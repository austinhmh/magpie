package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/yetone/magpie/internal/appdir"
)

// The Claude subscription bridge's MCP helper is magpie's own executable,
// run by Claude Code as `magpie claude-mcp-helper` (serveClaudeSubscription
// builds the mcp-config with claudeCLI.exe()). magpie starts it from a copy
// of its own executable kept under the cache, not from the installed path,
// so the file a helper runs from is one magpie wrote once and nothing else
// replaces.
//
// On a Mac with an endpoint-security agent, a run started from the installed
// binary at ~/.local/bin was killed before it ran whenever that file had
// just been replaced: a restart script swapped in a fresh copy on every
// launch, so the agent re-evaluated a new executable each time and, under
// its own load, left the process stuck in exec. Every MCP helper then failed
// to connect and the model was offered none of the client's tools. A copy
// made once here and reused unchanged is presented to such an agent once.
// This does not disable, bypass or hide from the agent; it only stops magpie
// from handing it a new executable on every launch. See
// docs/reclaude-local-bridge.md.

// selfCopy is magpie's own executable, copied once per process to a stable
// path under the cache, used as the MCP helper command. On any error the
// caller falls back to the executable itself.
var selfCopy = sync.OnceValues(func() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return copyExeToCache(exe, appdir.Cache())
})

// copyExeToCache copies the executable at src into a folder under cacheRoot
// named for its content, and returns the copy's path. A copy already there
// with the same size is reused as it is, so a restart neither rewrites the
// file nor changes its inode: an endpoint-security agent that judged the
// copy once is not handed a new file to judge.
func copyExeToCache(src, cacheRoot string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	sum := sha256.New()
	if _, err := io.Copy(sum, in); err != nil {
		return "", err
	}
	tag := hex.EncodeToString(sum.Sum(nil))[:12]
	dir := filepath.Join(cacheRoot, "claude-helper", tag)
	dst := filepath.Join(dir, filepath.Base(src))
	if fi, err := os.Stat(dst); err == nil && fi.Size() == info.Size() {
		return dst, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(src)+".*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	remove := func() { _ = os.Remove(tmpName) }
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		remove()
		return "", err
	}
	if err := tmp.Chmod(0o755); err != nil {
		_ = tmp.Close()
		remove()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		remove()
		return "", err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		remove()
		return "", err
	}
	return dst, nil
}
