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
    -- Advisory spend ceiling. Exceeding it raises a banner on the dashboard
    -- and nothing else: killing a task mid-turn would abandon the worktree in
    -- a half-written state and waste everything already paid for. Zero means
    -- no cap.
    cost_cap_usd      REAL NOT NULL DEFAULT 0,
    created_at        TEXT NOT NULL,
    updated_at        TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_tasks_state ON tasks(state);

-- A task waits for every row here to reach done before a worker may claim it.
-- Self-references and cycles are rejected on write, so the scheduler can trust
-- that a queued task with no unmet dependency is always claimable.
CREATE TABLE IF NOT EXISTS task_deps (
    task_id       INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    depends_on_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    PRIMARY KEY (task_id, depends_on_id)
);

CREATE INDEX IF NOT EXISTS idx_task_deps_dependee ON task_deps(depends_on_id);

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

-- A repository analysis and the task list it proposed.
--
-- The draft lives here rather than in the browser because the dashboard
-- reloads on every state event: anything held only in the DOM would be thrown
-- away several times a minute while other tasks run. Keeping it in the
-- database also means an analysis survives a daemon restart, and that the
-- money it cost is visible rather than spent invisibly.
CREATE TABLE IF NOT EXISTS proposals (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_path       TEXT NOT NULL DEFAULT '',
    source_url      TEXT NOT NULL DEFAULT '',
    -- draft | cloning | analysing | ready | queued | discarded | failed
    state           TEXT NOT NULL,
    focus           TEXT NOT NULL DEFAULT '',
    notes           TEXT NOT NULL DEFAULT '',
    max_tasks       INTEGER NOT NULL DEFAULT 12,
    model           TEXT NOT NULL DEFAULT '',
    detected        TEXT NOT NULL DEFAULT '',
    -- Accumulated across regenerates: a re-ask really did cost what the first
    -- attempt cost plus the second.
    cost_usd        REAL NOT NULL DEFAULT 0,
    input_tokens    INTEGER NOT NULL DEFAULT 0,
    output_tokens   INTEGER NOT NULL DEFAULT 0,
    transcript_path TEXT NOT NULL DEFAULT '',
    err_msg         TEXT NOT NULL DEFAULT '',
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_proposals_state ON proposals(state);

-- One proposed task. `key` is the model's own local name for it, which is what
-- depends_on refers to: the real slug is not known until the task is created,
-- and Submit suffixes it on collision.
CREATE TABLE IF NOT EXISTS proposal_tasks (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    proposal_id     INTEGER NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
    ord             INTEGER NOT NULL,
    key             TEXT NOT NULL,
    goal            TEXT NOT NULL,
    constraints     TEXT NOT NULL DEFAULT '',
    verify          TEXT NOT NULL DEFAULT '',
    severity        TEXT NOT NULL DEFAULT 'any',
    cost_cap        REAL NOT NULL DEFAULT 0,
    depends_on      TEXT NOT NULL DEFAULT '',
    rationale       TEXT NOT NULL DEFAULT '',
    evidence        TEXT NOT NULL DEFAULT '',
    selected        INTEGER NOT NULL DEFAULT 1,
    created_task_id INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_proposal_tasks_proposal ON proposal_tasks(proposal_id, ord);
