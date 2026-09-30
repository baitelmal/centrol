//go:build windows

package gate

import "os"

// OpenControllingTTY is the Windows equivalent of the unix /dev/tty
// opener: CONIN$/CONOUT$ address the console regardless of stdio
// redirection. We only need read+write on one handle for the simple
// line-based prompts this package renders.
func OpenControllingTTY() (*os.File, error) {
	return os.OpenFile("CONIN$", os.O_RDWR, 0)
}
