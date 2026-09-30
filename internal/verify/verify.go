// Package verify is reserved for the v0.3 validator surface: attempt →
// test → invariant-check → packet → approve/reject, gated through
// internal/gate with the [Approve | Modify | Reject] option set and
// logged through the same Governor as guard and proxy, using the
// verify.* event types and the kind=verify Contract shape already
// reserved in the schema and internal/policy respectively.
//
// Nothing in this package is wired to any command yet. It exists so
// that shipping v0.3 is additive — a new command plus real logic behind
// this stub — rather than a schema or contract redesign.
package verify

import "fmt"

// ErrNotImplemented is returned by every entry point in this package
// until v0.3 lands.
var ErrNotImplemented = fmt.Errorf("verify: not yet implemented")

// Run is the reserved entry point `centrol verify` will call. It always
// returns ErrNotImplemented today.
func Run(args []string) error {
	return ErrNotImplemented
}
