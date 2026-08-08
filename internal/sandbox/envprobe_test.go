package sandbox

import (
	"os/exec"
	"strings"
	"testing"
)

// bubblewrap clears the environment and rebuilds it from the spec, so a
// variable the engine sets is only useful if it survives that wipe. This is
// the end-to-end statement of it: the marker that stops an agent nesting its
// own sandbox has to actually arrive in the confined process.
func TestSpecEnvReachesTheConfinedProcess(t *testing.T) {
	if err := Probe("bwrap"); err != nil {
		t.Skip("bwrap unusable here")
	}
	spec := Spec{
		HomeDir: t.TempDir(),
		WorkDir: "/",
		PathEnv: "/usr/bin:/bin",
		Env:     map[string]string{"CLAUDE_CODE_SANDBOXED": "1"},
	}
	for _, dir := range []string{"/usr", "/bin", "/lib", "/lib64", "/etc"} {
		spec = spec.AddOptional(dir, false)
	}

	bin, argv := Bwrap{Bin: "bwrap"}.Wrap("/bin/sh",
		[]string{"-c", `printf %s "$CLAUDE_CODE_SANDBOXED"`}, spec)
	out, err := exec.Command(bin, argv...).CombinedOutput()
	if err != nil {
		t.Fatalf("run: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "1" {
		t.Fatalf("the confined process saw CLAUDE_CODE_SANDBOXED=%q, want 1", got)
	}
}
