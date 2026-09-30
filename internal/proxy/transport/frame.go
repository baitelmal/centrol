package transport

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
)

// readFrames reads newline-delimited JSON-RPC messages from r and
// sends each raw line (without its trailing newline) on the returned
// channel, which is closed when r is exhausted or errors. The
// returned errFn must be called only after the channel has been fully
// drained; it reports nil for a clean EOF, or a non-nil error for a
// real scan failure (bufio.Scanner.Err()) — except when that failure
// is os.ErrClosed, which specifically means the underlying *os.File
// was closed out from under this read. That happens, by design, when
// the reader is an exec.Cmd's StdoutPipe and the caller's cmd.Wait()
// has already seen the child exit: per os/exec's own documented
// contract, "Wait will close the pipe after seeing the command exit."
// That is the target process ending normally, observed as a
// close-during-read race instead of a tidy EOF — not a real transport
// failure and not data loss — so it is reported the same as a clean
// EOF rather than as a read error.
//
// This mirrors proxy.ReadFrames (internal/proxy/proxy.go) exactly;
// duplicated here rather than shared so this package has no
// dependency on package proxy, which depends on this one.
func readFrames(r io.Reader) (out <-chan []byte, errFn func() error) {
	ch := make(chan []byte)
	var scanErr error
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			cp := make([]byte, len(line))
			copy(cp, line)
			ch <- cp
		}
		if err := scanner.Err(); err != nil && !errors.Is(err, os.ErrClosed) {
			scanErr = err
		}
	}()
	return ch, func() error {
		if scanErr == nil {
			return nil
		}
		return fmt.Errorf("stream read error: %w", scanErr)
	}
}

// writeFrame writes exactly one JSON-RPC frame plus a trailing
// newline. Mirrors proxy.WriteFrame exactly, for the same reason
// readFrames mirrors proxy.ReadFrames.
func writeFrame(w io.Writer, b []byte) error {
	if _, err := w.Write(b); err != nil {
		return err
	}
	_, err := w.Write([]byte("\n"))
	return err
}
