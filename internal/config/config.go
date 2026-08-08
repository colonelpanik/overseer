// Package config loads the overseer daemon's settings.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds daemon-wide settings. Batch files submitted with
// `overseer submit` carry tasks only, never these values, so submitting a
// second batch cannot change the concurrency of a run already in flight.
type Config struct {
	ListenAddr       string        `yaml:"listen_addr"`
	DataDir          string        `yaml:"data_dir"`
	MaxParallel      int           `yaml:"max_parallel"`
	MaxIterations    int           `yaml:"max_iterations"`
	StepTimeout      time.Duration `yaml:"step_timeout"`
	BlockingSeverity string        `yaml:"blocking_severity"`
	ClaudeBin        string        `yaml:"claude_bin"`
	CodexBin         string        `yaml:"codex_bin"`
	GhBin            string        `yaml:"gh_bin"`

	// VerifyCommand is run in the worktree after each implementation turn and
	// must exit zero before the code review happens. Empty disables the gate,
	// which leaves convergence meaning only that Codex stopped objecting.
	VerifyCommand string `yaml:"verify_command"`

	// RunCapUSD and TaskCapUSD are advisory spend ceilings for the whole run
	// and for one task. Passing either raises a banner on the dashboard and
	// changes nothing else: overseer will not kill an agent mid-edit, because
	// that abandons the worktree in a state nobody chose and throws away the
	// money already spent getting there. Zero means no cap.
	RunCapUSD  float64 `yaml:"run_cap_usd"`
	TaskCapUSD float64 `yaml:"task_cap_usd"`

	// AnalysisModel drives the repository analysis behind the dashboard's
	// wizard. It is a separate knob from whatever model runs the loop: the
	// analysis reads a repository once and writes nothing, so it is worth
	// spending less on than the turns that produce the actual change.
	AnalysisModel string `yaml:"analysis_model"`

	// Sandbox is auto, bwrap or off. auto uses bwrap when it works and
	// warns loudly when it does not.
	Sandbox  string `yaml:"sandbox"`
	BwrapBin string `yaml:"bwrap_bin"`
	// SandboxCachePaths are toolchain caches mounted writable. They are
	// derived data, and without them a $HOME tmpfs forces a cold build cache
	// and a full dependency re-download on every agent turn.
	SandboxCachePaths []string `yaml:"sandbox_cache_paths"`
	// SandboxExtraReadOnly and SandboxExtraReadWrite are operator escape
	// hatches for what the defaults cannot know about. Both are empty by
	// default: mounting credentials hands them to the agent.
	SandboxExtraReadOnly  []string `yaml:"sandbox_extra_read_only"`
	SandboxExtraReadWrite []string `yaml:"sandbox_extra_read_write"`
	// SandboxEnvPassthrough names additional environment variables let through
	// to the sandboxed agent, beyond the fixed allowlist (HOME, PATH, TERM,
	// LANG/LC_*, and the agents' own ANTHROPIC_*/CLAUDE_*/CODEX_*/
	// OPENAI_API_KEY credentials). Empty by default: bubblewrap clears the
	// daemon's environment before the agent runs, and inheritance from there
	// is opt-in, not opt-out.
	SandboxEnvPassthrough []string `yaml:"sandbox_env_passthrough"`
}

// ValidSeverities are the accepted blocking-severity thresholds, loosest first.
var ValidSeverities = []string{"any", "minor", "major", "critical"}

// Default returns the configuration used when no file is present.
func Default() Config {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return Config{
		ListenAddr:       "127.0.0.1:7777",
		DataDir:          filepath.Join(home, ".overseer"),
		MaxParallel:      3,
		MaxIterations:    10,
		StepTimeout:      30 * time.Minute,
		BlockingSeverity: "any",
		ClaudeBin:        "claude",
		CodexBin:         "codex",
		GhBin:            "gh",
		Sandbox:          "auto",
		BwrapBin:         "bwrap",
		AnalysisModel:    "claude-sonnet-5",
		// These cover the common toolchains; a path that does not exist is
		// skipped, so listing several is harmless.
		//
		// The Go build cache and module cache are deliberately NOT here. Go's
		// build cache holds trusted, reused-verbatim output blobs (full
		// verification is opt-in behind GODEBUG=gocacheverify=1), so a write an
		// agent smuggled through the operator's real ~/.cache/go-build would
		// survive the sandbox and could get linked into a later *unsandboxed*
		// build on the operator's own machine. The engine gives the agent its
		// own Go caches under overseer's data directory instead, wired up with
		// GOCACHE/GOMODCACHE, which keeps the same speed benefit across
		// iterations of one run without exposing anything of the operator's.
		SandboxCachePaths: []string{
			"$HOME/.cargo/registry",
			"$HOME/.npm",
			"$HOME/.cache/pip",
		},
	}
}

// Load reads path over the top of Default. A missing file is not an error.
func Load(path string) (Config, error) {
	c := Default()
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("read config: %w", err)
	}
	// Unmarshalling into the already-populated struct leaves absent keys at
	// their default values.
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse config: %w", err)
	}
	if err := c.validate(); err != nil {
		return c, err
	}
	return c, nil
}

func (c Config) validate() error {
	found := false
	for _, s := range ValidSeverities {
		if c.BlockingSeverity == s {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("blocking_severity %q must be one of %v", c.BlockingSeverity, ValidSeverities)
	}

	switch c.Sandbox {
	case "auto", "bwrap", "off":
	default:
		return fmt.Errorf("sandbox %q must be auto, bwrap or off", c.Sandbox)
	}

	if c.RunCapUSD < 0 {
		return fmt.Errorf("run_cap_usd %.2f must not be negative", c.RunCapUSD)
	}
	if c.TaskCapUSD < 0 {
		return fmt.Errorf("task_cap_usd %.2f must not be negative", c.TaskCapUSD)
	}
	return nil
}

// RunsDir is where per-step agent transcripts are written.
func (c Config) RunsDir() string { return filepath.Join(c.DataDir, "runs") }

// DBPath is the SQLite file.
func (c Config) DBPath() string { return filepath.Join(c.DataDir, "overseer.db") }

// WorktreesDir is where task worktrees are created.
func (c Config) WorktreesDir() string { return filepath.Join(c.DataDir, "worktrees") }

// ProposalsDir is where a repository analysis writes its transcript and the
// per-run agent state that stands in for the agent's real one.
func (c Config) ProposalsDir() string { return filepath.Join(c.DataDir, "proposals") }

// ImportsDir is where a repository cloned from a URL lands.
func (c Config) ImportsDir() string { return filepath.Join(c.DataDir, "imported") }
