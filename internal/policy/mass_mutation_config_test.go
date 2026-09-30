package policy

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateMassMutationThresholdBounds(t *testing.T) {
	if err := ValidateMassMutationThreshold(20); err != nil {
		t.Fatalf("expected 20 to be valid, got %v", err)
	}
	if err := ValidateMassMutationThreshold(10); err != nil {
		t.Fatalf("expected the floor 10 to be valid, got %v", err)
	}
	if err := ValidateMassMutationThreshold(1000); err != nil {
		t.Fatalf("expected the ceiling 1000 to be valid, got %v", err)
	}
	if err := ValidateMassMutationThreshold(5); err == nil {
		t.Fatalf("expected 5 (below floor) to be rejected")
	}
	if err := ValidateMassMutationThreshold(2000); err == nil {
		t.Fatalf("expected 2000 (above ceiling) to be rejected")
	}
}

// TestSection1Acceptance mirrors the finalization pass's exact
// acceptance criteria: user config 50 changes behavior, 5 is rejected,
// 2000 is rejected.
func TestSection1Acceptance(t *testing.T) {
	dir := t.TempDir()
	user := NewLocalFileSource(SourceUserConfig, filepath.Join(dir, "user.toml"))
	def := NewDefaultSource(nil)

	// 50 changes prompt behavior in a test run.
	if err := user.Write("proxy.mass_mutation_threshold", 50); err != nil {
		t.Fatal(err)
	}
	r := NewResolver(NewSessionContractSource(), NewEnterpriseSource(), user, def)
	n, source, err := ResolveMassMutationThreshold(r)
	if err != nil {
		t.Fatalf("ResolveMassMutationThreshold: %v", err)
	}
	if n != 50 || source != SourceUserConfig {
		t.Fatalf("expected 50 from user_config, got %d from %s", n, source)
	}
	c := Contract{MassMutationThreshold: n}
	tier, _ := EvaluateMassMutation(45, c)
	if tier != Allow {
		t.Fatalf("expected 45 files to be Allow under threshold 50, got %v", tier)
	}
	tier, _ = EvaluateMassMutation(51, c)
	if tier != Flag {
		t.Fatalf("expected 51 files to Flag under threshold 50, got %v", tier)
	}

	// 5 is rejected with a clear error.
	if err := user.Write("proxy.mass_mutation_threshold", 5); err != nil {
		t.Fatal(err)
	}
	_, _, err = ResolveMassMutationThreshold(r)
	if err == nil {
		t.Fatalf("expected 5 to be rejected")
	}
	if !strings.Contains(err.Error(), "5") || !strings.Contains(err.Error(), "10") {
		t.Fatalf("expected a clear error naming both the bad value and the floor, got: %v", err)
	}

	// 2000 is rejected with a clear error.
	if err := user.Write("proxy.mass_mutation_threshold", 2000); err != nil {
		t.Fatal(err)
	}
	_, _, err = ResolveMassMutationThreshold(r)
	if err == nil {
		t.Fatalf("expected 2000 to be rejected")
	}
	if !strings.Contains(err.Error(), "2000") || !strings.Contains(err.Error(), "1000") {
		t.Fatalf("expected a clear error naming both the bad value and the ceiling, got: %v", err)
	}
}

func TestResolveMassMutationThresholdDefaultsWhenUnset(t *testing.T) {
	r := NewResolver(NewDefaultSource(nil))
	n, source, err := ResolveMassMutationThreshold(r)
	if err != nil {
		t.Fatalf("ResolveMassMutationThreshold: %v", err)
	}
	if n != DefaultMassMutationThreshold || source != SourceDefault {
		t.Fatalf("expected default %d, got %d from %s", DefaultMassMutationThreshold, n, source)
	}
}

func TestMassMutationLogLineFormat(t *testing.T) {
	c := Contract{MassMutationThreshold: 20}
	_, reason := EvaluateMassMutation(23, c)
	want := "mass-mutation threshold (20) exceeded: 23 files touched in one call"
	if reason != want {
		t.Fatalf("expected exact log format %q, got %q", want, reason)
	}
}
