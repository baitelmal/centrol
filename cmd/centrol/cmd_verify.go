package main

import (
	"fmt"
	"os"

	"github.com/scirem/centrol/internal/verify"
)

// cmdVerify is the reserved entry point for the v0.3 validator surface.
// It always reports not-yet-implemented and exits non-zero; see
// internal/verify for what's already reserved in the schema and
// contract shapes ahead of it.
func cmdVerify(args []string) {
	if err := verify.Run(args); err != nil {
		fmt.Fprintf(os.Stderr, "centrol verify: %v\n", err)
		os.Exit(1)
	}
}
