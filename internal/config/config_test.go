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
