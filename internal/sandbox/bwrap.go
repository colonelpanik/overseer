package sandbox

import "sort"

// Bwrap confines commands with bubblewrap.
type Bwrap struct {
	Bin string
}

// Name identifies the mode.
func (Bwrap) Name() string { return "bwrap" }

// Wrap builds a bubblewrap invocation.
//
// The shape is: a read-only system, an empty tmpfs over $HOME, then the
// caller's mounts re-exposing exactly what the agent needs. Network is
// deliberately shared — the agents call an HTTP API — so this constrains the
// filesystem, not exfiltration.
func (b Bwrap) Wrap(bin string, args []string, spec Spec) (string, []string) {
	out := []string{
		// Wipe the daemon's own environment before anything else. Without
		// this, bubblewrap passes its own environment straight through to the
		// sandboxed process — GITHUB_TOKEN, AWS_*, and whatever else the
		// operator's shell exports would all reach the agent despite the
		// filesystem sandbox around it. Must precede every --setenv below:
		// bubblewrap applies environment operations in argument order, so a
		// --clearenv appearing after a --setenv would wipe that variable too.
		"--clearenv",
		// A read-only system. /bin, /sbin, /lib and /lib64 are symlinks into
		// /usr on this distribution layout; --symlink always succeeds, so
		// this is harmless where they are real directories too.
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/etc", "/etc",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--tmpfs", "/run",
		// /etc/resolv.conf is a symlink into /run on systemd-resolved hosts,
		// so the tmpfs above breaks DNS unless the real file is re-exposed.
		// -try because not every host runs systemd-resolved.
		"--ro-bind-try", "/run/systemd/resolve", "/run/systemd/resolve",
	}

	if spec.HomeDir != "" {
		// Everything in $HOME disappears; Mounts decide what comes back.
		out = append(out, "--tmpfs", spec.HomeDir)
	}

	for _, m := range spec.Mounts {
		flag := "--ro-bind"
		if m.Write {
			flag = "--bind"
		}
		if m.Optional {
			// Let bubblewrap decide at mount time. Checking existence here
			// instead would race: the path could vanish between the check and
			// the mount, turning a skipped mount into a hard failure.
			flag += "-try"
		}
		out = append(out, flag, m.Src, m.Dest)
	}

	if spec.HomeDir != "" {
		out = append(out, "--setenv", "HOME", spec.HomeDir)
	}
	if spec.PathEnv != "" {
		out = append(out, "--setenv", "PATH", spec.PathEnv)
	}
	// Sorted for a deterministic argv, which matters for tests asserting on
	// it and for anyone diffing two runs' invocations.
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, "--setenv", k, spec.Env[k])
	}
	if spec.WorkDir != "" {
		out = append(out, "--chdir", spec.WorkDir)
	}

	out = append(out,
		"--unshare-pid",
		"--unshare-ipc",
		"--unshare-uts",
		// Ensures the agent's children die with bwrap, so the step timeout's
		// process-group kill cannot leak a running agent.
		"--die-with-parent",
		"--new-session",
		"--",
	)
	out = append(out, bin)
	out = append(out, args...)
	return b.Bin, out
}
