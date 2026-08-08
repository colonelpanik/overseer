-- A repository overseer works on. Registered once, so a path is typed once and
-- everything — tasks, analyses, time, usage, the backlog — attributes to it.
--
-- The path is the identity, resolved to an absolute path before insert. The
-- slug is only a short name for display and for naming a repo in a task file;
-- two repositories whose directories share a basename get a suffixed slug the
-- way colliding task slugs already do.
CREATE TABLE IF NOT EXISTS repos (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    slug           TEXT NOT NULL UNIQUE,
    path           TEXT NOT NULL UNIQUE,
    origin_url     TEXT NOT NULL DEFAULT '',
    default_branch TEXT NOT NULL DEFAULT '',
    detected       TEXT NOT NULL DEFAULT '',
    -- Defaults new tasks inherit. Resolution is task > repo > daemon default,
    -- so an empty value here means "fall through", not "off".
    verify_command    TEXT NOT NULL DEFAULT '',
    blocking_severity TEXT NOT NULL DEFAULT '',
    cost_cap_usd      REAL NOT NULL DEFAULT 0,
    archived_at    TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
);

-- A repository's durable todo list: things worth doing that nothing is doing
-- yet. Fed by analyses (proposed tasks nobody queued), by reviews (findings
-- below the blocking threshold, which the loop deliberately did not act on and
-- which had nowhere to go), and by hand.
CREATE TABLE IF NOT EXISTS backlog (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    repo_id     INTEGER NOT NULL REFERENCES repos(id) ON DELETE CASCADE,
    -- analysis | review | manual
    source      TEXT NOT NULL,
    title       TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    evidence    TEXT NOT NULL DEFAULT '',
    severity    TEXT NOT NULL DEFAULT '',
    -- fingerprint collapses the same item raised repeatedly into one row with
    -- a count. A nit the reviewer raises on three separate tasks is one thing
    -- to fix, and "seen three times" is a far stronger signal than three
    -- identical rows.
    fingerprint TEXT NOT NULL,
    seen        INTEGER NOT NULL DEFAULT 1,
    -- Where it came from, so an item can be traced back.
    proposal_task_id INTEGER NOT NULL DEFAULT 0,
    finding_id       INTEGER NOT NULL DEFAULT 0,
    origin_task_id   INTEGER NOT NULL DEFAULT 0,
    -- open | queued | dismissed
    state           TEXT NOT NULL,
    created_task_id INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_backlog_fingerprint ON backlog(repo_id, fingerprint);
CREATE INDEX IF NOT EXISTS idx_backlog_repo ON backlog(repo_id, state);

-- Daemon-level state that must survive a restart. Currently one key: the
-- operator's global stop. The auth-failure pause is deliberately NOT stored
-- here — it is a reaction to a condition that may have cleared, and a restart
-- is exactly when it should be retried.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS tasks (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    slug              TEXT NOT NULL UNIQUE,
    -- The registered repository, and the resolved path it points at. The path
    -- stays so every existing reader keeps working; repo_id is what makes a
    -- repository's history add up.
    repo_id           INTEGER NOT NULL DEFAULT 0,
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
    -- When the operator stopped this task, or empty. Deliberately not a state:
    -- the state column names the action that was in flight, which is what
    -- loop.Pending re-dispatches on resume, so overwriting it would make a
    -- stopped task unable to say where it had got to.
    stopped_at        TEXT NOT NULL DEFAULT '',
    -- Which attempt this is. A restart bumps it, and the worktree, branch,
    -- transcripts and agent state are all keyed by it, so one attempt cannot
    -- append to another's.
    run_seq           INTEGER NOT NULL DEFAULT 1,
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
    -- Which configured provider served this step, so subscription-covered CLI
    -- usage and usage metered against an endpoint the operator supplied can be
    -- told apart rather than added into one misleading figure.
    provider        TEXT NOT NULL DEFAULT '',
    -- Which attempt of the task this step belongs to, so a restarted task's
    -- history stays separable from the attempt before it.
    run_seq         INTEGER NOT NULL DEFAULT 1,
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
    repo_id         INTEGER NOT NULL DEFAULT 0,
    repo_path       TEXT NOT NULL DEFAULT '',
    source_url      TEXT NOT NULL DEFAULT '',
    -- draft | cloning | analysing | ready | queued | discarded | failed
    state           TEXT NOT NULL,
    focus           TEXT NOT NULL DEFAULT '',
    notes           TEXT NOT NULL DEFAULT '',
    max_tasks       INTEGER NOT NULL DEFAULT 12,
    model           TEXT NOT NULL DEFAULT '',
    detected        TEXT NOT NULL DEFAULT '',
    provider        TEXT NOT NULL DEFAULT '',
    -- analyse | create. What the wizard is doing: reading a repository that
    -- exists, or designing and building one that does not. Every row written
    -- before this column existed is an analyse.
    kind            TEXT NOT NULL DEFAULT 'analyse',
    -- The design the architect and the operator arrived at together. Held here
    -- rather than written into the repository during the conversation: an
    -- existing repository is mounted read-only for exactly the reason that
    -- nothing should be left behind in a tree somebody only asked us to read.
    design          TEXT NOT NULL DEFAULT '',
    -- The agent session the conversation resumes into, so each turn continues
    -- the last rather than starting over.
    architect_session TEXT NOT NULL DEFAULT '',
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

-- One turn of the architect conversation: the operator thinking out loud with
-- an agent before any task exists.
--
-- Not on `steps`, which is NOT NULL REFERENCES tasks(id) — this happens before
-- there is a task, and is the thing that decides what the tasks are.
CREATE TABLE IF NOT EXISTS architect_turns (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    proposal_id INTEGER NOT NULL REFERENCES proposals(id) ON DELETE CASCADE,
    -- operator | architect
    speaker     TEXT NOT NULL,
    body        TEXT NOT NULL,
    -- Usage, on the architect's turns only. A conversation costs real turns and
    -- the wizard should say so before the operator has had ten of them.
    cost_usd      REAL NOT NULL DEFAULT 0,
    input_tokens  INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    err_msg     TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_architect_turns_proposal ON architect_turns(proposal_id, id);
