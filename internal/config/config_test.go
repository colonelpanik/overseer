package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultHasUsableValues(t *testing.T) {
	c := Default()
	if c.MaxParallel != 3 {
		t.Errorf("MaxParallel = %d, want 3", c.MaxParallel)
	}
	if c.MaxIterations != 10 {
		t.Errorf("MaxIterations = %d, want 10", c.MaxIterations)
	}
	if c.StepTimeout != 30*time.Minute {
		t.Errorf("StepTimeout = %v, want 30m", c.StepTimeout)
	}
	if c.BlockingSeverity != "any" {
		t.Errorf("BlockingSeverity = %q, want \"any\"", c.BlockingSeverity)
	}
}

func TestLoadOverridesOnlyPresentKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "max_parallel: 7\nstep_timeout: 5m\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxParallel != 7 {
		t.Errorf("MaxParallel = %d, want 7", c.MaxParallel)
	}
	if c.StepTimeout != 5*time.Minute {
		t.Errorf("StepTimeout = %v, want 5m", c.StepTimeout)
	}
	if c.MaxIterations != 10 {
		t.Errorf("MaxIterations = %d, want default 10", c.MaxIterations)
	}
}

func TestLoadMissingFileReturnsDefaults(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("Load of missing file should not error: %v", err)
	}
	if c.MaxParallel != 3 {
		t.Errorf("MaxParallel = %d, want 3", c.MaxParallel)
	}
}

func TestLoadRejectsUnknownSeverity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("blocking_severity: whatever\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown blocking_severity")
	}
}

func TestSandboxDefaultsToAuto(t *testing.T) {
	if c := Default(); c.Sandbox != "auto" {
		t.Errorf("Sandbox = %q, want auto", c.Sandbox)
	}
}

func TestLoadRejectsUnknownSandboxMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("sandbox: yolo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected an error for an unknown sandbox mode")
	}
}

func TestAnalysisTimeoutIsConfigurableAndNotStricterThanAStep(t *testing.T) {
	// It was a hardcoded 15 minutes — shorter than the timeout a coding turn
	// gets — which failed on exactly the large unfamiliar repository the
	// wizard exists for, with no knob to turn.
	d := Default()
	if d.AnalysisTimeout < d.StepTimeout {
		t.Errorf("analysis_timeout %s is stricter than step_timeout %s",
			d.AnalysisTimeout, d.StepTimeout)
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("analysis_timeout: 90m\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.AnalysisTimeout != 90*time.Minute {
		t.Errorf("AnalysisTimeout = %s, want 90m", c.AnalysisTimeout)
	}

	// Zero would mean "no time at all" to the runner, so it is refused rather
	// than silently treated as a default.
	if err := os.WriteFile(path, []byte("analysis_timeout: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("a zero analysis_timeout should be refused")
	}
}

func TestLoadRejectsNonPositiveMaxIterations(t *testing.T) {
	// max_iterations is the per-phase iteration budget, not one of the
	// advisory "0 disables" spend caps: a task handed zero iterations parks
	// for a human before it does any work, so the value is refused at load
	// rather than quietly swapped for the default.
	for _, tc := range []struct{ name, body string }{
		{"zero", "max_iterations: 0\n"},
		{"negative", "max_iterations: -1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Errorf("a %s max_iterations should be refused", tc.name)
			}
		})
	}

	// A positive value the operator states explicitly is still honoured.
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("max_iterations: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxIterations != 3 {
		t.Errorf("MaxIterations = %d, want 3", c.MaxIterations)
	}
}
