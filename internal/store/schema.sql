CREATE TABLE IF NOT EXISTS tasks (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    slug              TEXT NOT NULL UNIQUE,
    repo_path         TEXT NOT NULL,
    goal              TEXT NOT NULL,
    constraints       TEXT NOT NULL DEFAULT '',
    state             TEXT NOT NULL,
    phase             TEXT NOT NULL DEFAULT '',
    iteration         INTEGER NOT NULL DEFAULT 0,
    max_iterations    INTEGER NOT NULL DEFAULT 10,
    blocking_severity TEXT NOT NULL DEFAULT 'any',
    plan_session_id   TEXT NOT NULL DEFAULT '',
    exec_session_id   TEXT NOT NULL DEFAULT '',
    branch            TEXT NOT NULL DEFAULT '',
    base_ref          TEXT NOT NULL DEFAULT '',
    git_common_dir    TEXT NOT NULL DEFAULT '',
    git_admin_dir     TEXT NOT NULL DEFAULT '',
    worktree_dir      TEXT NOT NULL DEFAULT '',
    pr_url            TEXT NOT NULL DEFAULT '',
    err_msg           TEXT NOT NULL DEFAULT '',
    verify_command    TEXT NOT NULL DEFAULT '',
    finding_hashes    TEXT NOT NULL DEFAULT '',
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);

CREATE TABLE IF NOT EXISTS steps (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    task_id         INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    phase           TEXT NOT NULL,
    iteration       INTEGER NOT NULL,
    agent           TEXT NOT NULL,
    state           TEXT NOT NULL,
    started_at      TEXT NOT NULL,
    ended_at        TEXT NOT NULL DEFAULT '',
    exit_code       INTEGER NOT NULL DEFAULT 0,
    verdict         TEXT NOT NULL DEFAULT '',
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_usd        REAL NOT NULL DEFAULT 0,
    transcript_path TEXT NOT NULL DEFAULT '',
    err_msg         TEXT NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_steps_task ON steps(task_id, id);

CREATE TABLE IF NOT EXISTS findings (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    step_id            INTEGER NOT NULL REFERENCES steps(id) ON DELETE CASCADE,
    severity           TEXT NOT NULL,
    file               TEXT NOT NULL DEFAULT '',
    line               INTEGER NOT NULL DEFAULT 0,
    summary            TEXT NOT NULL,
    -- Volatile supplementary output (verify command tails). Stored separately
    -- from summary because the oscillation fingerprint hashes summary only,
    -- and this must survive a restart so a resumed turn keeps its context.
    detail             TEXT NOT NULL DEFAULT '',
    blocking           INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_findings_step ON findings(step_id);
