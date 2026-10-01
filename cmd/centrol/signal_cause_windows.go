//go:build windows

package main

// signalCauseFromErr always reports ok=false on Windows: race (b)
// (Pass 3.9 Section 5) is specifically a unix process-group phenomenon
// — a subprocess inheriting centrol's process group and being killed
// directly by a signal sent to that group. Windows has no equivalent
// of that propagation (see internal/terminal/forward_windows.go's own
// omission of SIGHUP for the same underlying reason: the platform has
// no matching primitive), so a snapshot failure on Windows is always
// an ordinary snapshot failure, never a signal racing the early signal
// handler.
func signalCauseFromErr(err error) (cause string, code int, ok bool) {
	return "", 0, false
}
