// Package gate implements the one shared Operational/Governance Mode
// transition mechanism used across surfaces:
//
//	Operational Mode — the agent is running, acting through guard/proxy.
//	Governance Mode  — a human is reviewing one flagged action.
//
// The two modes are mutually exclusive: Prompt is synchronous, so
// nothing about the run proceeds while a human decision is pending, and
// the run returns to Operational Mode the instant Prompt returns.
//
// The gate itself is generic — it renders whatever options the caller
// supplies and returns which one was chosen. It does not know what it
// is gating: guard and proxy supply [Allow once | Allow session |
// Deny]; the v0.3 verify surface supplies [Approve | Modify | Reject].
package gate

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Decision is the index into the Options slice the human chose. A
// human that gives unrecognized input, or an environment with no
// controlling terminal at all, yields NoDecision — every caller in this
// codebase treats NoDecision as fail-closed (deny/reject), never as an
// implicit allow.
type Decision int

const NoDecision Decision = -1

// Option pairs a full option label (matched case-insensitively in
// full, or by its first letter) with its display text. In practice
// label and display are the same string for every surface today, but
// keeping them separate lets a surface offer a short input token
// ("s") with a longer display label ("Allow session") without the
// matcher having to guess where the boundary is.
type Option struct {
	Label   string // what the human types to select this option (matched case-insensitively)
	Display string // what's shown in the rendered "[A | B | C]" prompt
}

// Opt is a convenience constructor for the common case where the
// display text and its first letter double as the match label.
func Opt(display string) Option {
	return Option{Label: display, Display: display}
}

// ErrPromptTimeout is returned by PromptWithTimeout when the timeout
// elapses before the human responds — distinct from NoDecision with a
// nil error (which means input arrived but didn't match any option),
// so a caller can log a timeout specifically rather than treating it
// as an ordinary denial.
var ErrPromptTimeout = fmt.Errorf("gate: prompt timed out waiting for a response")

// PromptWithTimeout is Prompt with a deadline: if no line has arrived
// from r within timeout, it returns (NoDecision, ErrPromptTimeout)
// immediately rather than continuing to block. Failing closed on
// timeout is the only safe default for an unattended flag prompt — the
// underlying read is abandoned (not canceled; the reading goroutine may
// still be blocked on r after this returns), which is an acceptable
// trade for how rarely this fires.
func PromptWithTimeout(r io.Reader, w io.Writer, title, detail string, options []Option, timeout time.Duration) (Decision, error) {
	type result struct {
		d   Decision
		err error
	}
	done := make(chan result, 1)
	go func() {
		d, err := Prompt(r, w, title, detail, options)
		done <- result{d, err}
	}()
	select {
	case res := <-done:
		return res.d, res.err
	case <-time.After(timeout):
		return NoDecision, ErrPromptTimeout
	}
}

// Prompt renders title/detail plus the given options on w as
// "<title>\n<detail>\n[Display1 | Display2 | ...]: ", reads one line of
// input from r, and matches it case-insensitively against each
// option's Label (in full) or first letter. It blocks until r yields a
// line or an error/EOF — this synchronous block is what makes the
// Operational/Governance transition real rather than notional: the
// caller must not act on the gated decision until Prompt returns.
func Prompt(r io.Reader, w io.Writer, title, detail string, options []Option) (Decision, error) {
	display := make([]string, len(options))
	for i, o := range options {
		display[i] = o.Display
	}
	if _, err := fmt.Fprintf(w, "\n%s\n%s\n[%s]: ", title, detail, strings.Join(display, " | ")); err != nil {
		return NoDecision, err
	}

	line, err := readOneLine(r)
	if err != nil && line == "" {
		return NoDecision, err
	}

	answer := strings.TrimSpace(strings.ToLower(line))
	if answer == "" {
		return NoDecision, nil
	}
	for i, o := range options {
		label := strings.ToLower(o.Label)
		if answer == label {
			return Decision(i), nil
		}
		if len(answer) == 1 && len(label) > 0 && rune(answer[0]) == rune(label[0]) {
			return Decision(i), nil
		}
	}
	return NoDecision, nil
}

// readOneLine reads up to and including the next '\n' (or EOF) one byte
// at a time via direct Read calls — deliberately not bufio.Reader, whose
// documented minimum internal buffer (16 bytes) would silently consume
// bytes belonging to whatever comes after this line on an interactive
// TTY or a piped multi-answer input stream. Trimmed of trailing
// newline/carriage-return.
func readOneLine(r io.Reader) (string, error) {
	var sb strings.Builder
	buf := make([]byte, 1)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if buf[0] == '\n' {
				break
			}
			sb.WriteByte(buf[0])
		}
		if err != nil {
			if err == io.EOF {
				return strings.TrimRight(sb.String(), "\r"), nil
			}
			return strings.TrimRight(sb.String(), "\r"), err
		}
	}
	return strings.TrimRight(sb.String(), "\r"), nil
}
