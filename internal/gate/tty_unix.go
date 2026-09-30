//go:build !windows

package gate

import "os"

// OpenControllingTTY opens the controlling terminal directly,
// independent of whatever stdin/stdout have been redirected to — the
// mechanism every FLAG-tier prompt in this codebase uses so it can ask
// a human even when the process's own stdio is a pipe (an MCP client's
// JSON-RPC stream, in proxy's case).
func OpenControllingTTY() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
