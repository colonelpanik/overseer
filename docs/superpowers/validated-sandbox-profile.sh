#!/bin/sh
# Validated bubblewrap profile, kept as evidence for Task 16 of the plan.
# Paths here are from the validation run; the real ones are built by
# internal/engine.sandboxSpec. Confirmed working: real 'claude -p' completes,
# its Bash tool writes to the real worktree, --resume recovers context, and
# 'codex exec -s read-only' runs nested. Confirmed blocked: ~/.bashrc, ~/.ssh,
# ~/code, and writes to ~/.claude/settings.json.
# Candidate overseer sandbox profile for the executing agent.
exec bwrap \
  --ro-bind /usr /usr \
  --ro-bind /etc /etc \
  --symlink usr/bin /bin --symlink usr/sbin /sbin \
  --symlink usr/lib /lib --symlink usr/lib64 /lib64 \
  --proc /proc --dev /dev \
  --tmpfs /tmp --tmpfs /run \
  --ro-bind /run/systemd/resolve /run/systemd/resolve \
  --tmpfs "/home/kal" \
  --ro-bind "/home/kal/.local/share/claude" "/home/kal/.local/share/claude" \
  --ro-bind "/home/kal/.local/bin" "/home/kal/.local/bin" \
  --bind "/home/kal/.claude" "/home/kal/.claude" \
  --ro-bind "/home/kal/.claude/settings.json" "/home/kal/.claude/settings.json" \
  --ro-bind "/home/kal/.claude/plugins" "/home/kal/.claude/plugins" \
  --bind "/home/kal/.claude.json" "/home/kal/.claude.json" \
  --ro-bind "/tmp/claude-1000/-home-kal-claude-start/d747fc87-a388-4342-902d-eeffef623447/scratchpad/sbx/repo/.git" "/tmp/claude-1000/-home-kal-claude-start/d747fc87-a388-4342-902d-eeffef623447/scratchpad/sbx/repo/.git" \
  --bind "/tmp/claude-1000/-home-kal-claude-start/d747fc87-a388-4342-902d-eeffef623447/scratchpad/sbx/repo/.git/worktrees/wt" "/tmp/claude-1000/-home-kal-claude-start/d747fc87-a388-4342-902d-eeffef623447/scratchpad/sbx/repo/.git/worktrees/wt" \
  --bind "/tmp/claude-1000/-home-kal-claude-start/d747fc87-a388-4342-902d-eeffef623447/scratchpad/sbx/wt" "/tmp/claude-1000/-home-kal-claude-start/d747fc87-a388-4342-902d-eeffef623447/scratchpad/sbx/wt" \
  --setenv HOME "/home/kal" \
  --setenv PATH "/home/kal/.local/bin:/usr/local/bin:/usr/bin:/bin" \
  --chdir "/tmp/claude-1000/-home-kal-claude-start/d747fc87-a388-4342-902d-eeffef623447/scratchpad/sbx/wt" \
  --unshare-pid --unshare-ipc --unshare-uts \
  --die-with-parent --new-session \
  "$@"
