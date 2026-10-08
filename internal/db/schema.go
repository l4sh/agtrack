package db

// SchemaVersion is the schema this build writes. v2 is the Python 1.0.0
// schema; v3 adds agent features and the spec entity.
const SchemaVersion = 4

// Statement lists, never split on semicolons (D15): trigger bodies contain
// semicolons of their own.

// schemaV2 is the Python build's schema, verbatim.
var schemaV2 = []string{
	`CREATE TABLE contexts (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    title       TEXT,
    archived    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
)`,
	`CREATE TABLE projects (
    id          INTEGER PRIMARY KEY,
    context_id  INTEGER NOT NULL REFERENCES contexts(id) ON DELETE CASCADE,
    parent_id   INTEGER REFERENCES projects(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    title       TEXT,
    archived    INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
)`,
	`CREATE UNIQUE INDEX idx_projects_unique
    ON projects(context_id, COALESCE(parent_id, 0), name)`,
	`CREATE INDEX idx_projects_parent ON projects(parent_id)`,
	`CREATE TABLE project_repos (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    slug        TEXT NOT NULL,
    url         TEXT,
    created_at  TEXT NOT NULL,
    UNIQUE(project_id, slug)
)`,
	`CREATE INDEX idx_project_repos_slug ON project_repos(slug)`,
	`CREATE TABLE tasks (
    id          INTEGER PRIMARY KEY,
    project_id  INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'todo'
                CHECK(status IN ('todo','in-progress','done','cancelled')),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    started_at  TEXT,
    closed_at   TEXT
)`,
	`CREATE INDEX idx_tasks_project ON tasks(project_id)`,
	`CREATE INDEX idx_tasks_status ON tasks(status)`,
	`CREATE TABLE subtasks (
    id          INTEGER PRIMARY KEY,
    task_id     INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    title       TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'todo'
                CHECK(status IN ('todo','in-progress','done','cancelled')),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    started_at  TEXT,
    closed_at   TEXT,
    UNIQUE(task_id, position)
)`,
	`CREATE INDEX idx_subtasks_task ON subtasks(task_id)`,
	`CREATE TABLE tags (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    created_at  TEXT NOT NULL
)`,
	`CREATE TABLE taggings (
    tag_id      INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    target_type TEXT NOT NULL CHECK(target_type IN ('project','task','subtask')),
    target_id   INTEGER NOT NULL,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (tag_id, target_type, target_id)
)`,
	`CREATE INDEX idx_taggings_target ON taggings(target_type, target_id)`,
	`CREATE TABLE notes (
    id          INTEGER PRIMARY KEY,
    target_type TEXT NOT NULL
                CHECK(target_type IN ('context','project','task','subtask','tag')),
    target_id   INTEGER NOT NULL,
    body        TEXT NOT NULL,
    actor       TEXT,
    created_at  TEXT NOT NULL
)`,
	`CREATE INDEX idx_notes_target ON notes(target_type, target_id)`,
	`CREATE TABLE pull_requests (
    id          INTEGER PRIMARY KEY,
    task_id     INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    url         TEXT NOT NULL,
    repo        TEXT,
    number      INTEGER,
    title       TEXT,
    state       TEXT NOT NULL DEFAULT 'open'
                CHECK(state IN ('open','draft','merged','closed')),
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL,
    UNIQUE(task_id, url)
)`,
	`CREATE INDEX idx_prs_task ON pull_requests(task_id)`,
	`CREATE TABLE events (
    id          INTEGER PRIMARY KEY,
    at          TEXT NOT NULL,
    recorded_at TEXT,
    actor       TEXT,
    target_type TEXT NOT NULL,
    target_id   INTEGER NOT NULL,
    action      TEXT NOT NULL,
    detail      TEXT,
    task_id     INTEGER,
    project_id  INTEGER
)`,
	`CREATE INDEX idx_events_at ON events(at)`,
	`CREATE INDEX idx_events_task ON events(task_id)`,
	`CREATE INDEX idx_events_project ON events(project_id)`,
	`CREATE TABLE meta (
    key         TEXT PRIMARY KEY,
    value       TEXT
)`,
}

// migration2 is v1 -> v2.
var migration2 = []string{
	`ALTER TABLE events ADD COLUMN recorded_at TEXT`,
	`UPDATE events SET recorded_at = at WHERE recorded_at IS NULL`,
}

// uidExpr generates a random RFC 4122 v4 uuid in SQL.
const uidExpr = `lower(hex(randomblob(4))) || '-' || lower(hex(randomblob(2))) || '-4' ||
 substr(lower(hex(randomblob(2))),2) || '-' ||
 substr('89ab', 1 + (abs(random()) % 4), 1) || substr(lower(hex(randomblob(2))),2) || '-' ||
 lower(hex(randomblob(6)))`

// NoteTargets are the polymorphic parents a note may hang off in v3.
var NoteTargets = []string{"context", "project", "task", "subtask", "tag", "spec", "spec_anchor"}

// NoteKinds are the values of notes.kind (design §4.7, go spec §9.4).
var NoteKinds = []string{"note", "decision", "handoff", "blocker", "question", "approval", "build-note"}

// migration3 is v2 -> v3: uid insurance, claims, notes.kind, deps,
// idempotency receipts, artifacts, and the spec entity with FTS5.
var migration3 = func() []string {
	s := []string{}
	for _, t := range []string{"contexts", "projects", "tasks", "subtasks", "events"} {
		s = append(s,
			`ALTER TABLE `+t+` ADD COLUMN uid TEXT`,
			`UPDATE `+t+` SET uid = `+uidExpr+` WHERE uid IS NULL`,
			`CREATE UNIQUE INDEX idx_`+t+`_uid ON `+t+`(uid)`,
		)
	}
	s = append(s,
		`ALTER TABLE tasks ADD COLUMN claimed_by TEXT`,
		`ALTER TABLE tasks ADD COLUMN claimed_at TEXT`,
		`ALTER TABLE tasks ADD COLUMN claim_expires_at TEXT`,

		// notes is rebuilt: the target_type CHECK widens (the one non-additive
		// change) and kind/resolution/threading/quote columns arrive. ids kept.
		`CREATE TABLE notes_v3 (
    id          INTEGER PRIMARY KEY,
    uid         TEXT UNIQUE,
    target_type TEXT NOT NULL
                CHECK(target_type IN ('context','project','task','subtask','tag','spec','spec_anchor')),
    target_id   INTEGER NOT NULL,
    body        TEXT NOT NULL,
    actor       TEXT,
    created_at  TEXT NOT NULL,
    kind        TEXT NOT NULL DEFAULT 'note'
                CHECK(kind IN ('note','decision','handoff','blocker','question','approval','build-note')),
    resolved_at TEXT,
    resolved_by TEXT,
    resolution  TEXT,
    parent_id   INTEGER REFERENCES notes(id) ON DELETE CASCADE,
    quote       TEXT,
    quote_prefix TEXT,
    quote_suffix TEXT,
    quote_ok    INTEGER,
    version_id  INTEGER,
    released_at TEXT,
    release_event INTEGER
)`,
		`INSERT INTO notes_v3 (id, uid, target_type, target_id, body, actor, created_at)
    SELECT id, `+uidExpr+`, target_type, target_id, body, actor, created_at FROM notes`,
		`DROP TABLE notes`,
		`ALTER TABLE notes_v3 RENAME TO notes`,
		`CREATE INDEX idx_notes_target ON notes(target_type, target_id)`,
		`CREATE INDEX idx_notes_parent ON notes(parent_id)`,

		`CREATE TABLE task_deps (
    task_id       INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    depends_on_id INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind          TEXT NOT NULL DEFAULT 'blocks'
                  CHECK(kind IN ('blocks','relates','duplicates')),
    reason        TEXT,
    actor         TEXT,
    created_at    TEXT NOT NULL,
    PRIMARY KEY (task_id, depends_on_id, kind)
)`,
		`CREATE INDEX idx_task_deps_on ON task_deps(depends_on_id)`,

		`CREATE TABLE idempotency (
    key         TEXT PRIMARY KEY,
    target_type TEXT NOT NULL,
    target_id   INTEGER NOT NULL,
    payload_sha TEXT NOT NULL,
    output      TEXT,
    actor       TEXT,
    created_at  TEXT NOT NULL
)`,

		`CREATE TABLE artifacts (
    id          INTEGER PRIMARY KEY,
    task_id     INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL
                CHECK(kind IN ('branch','commit','file','url','run','worktree')),
    value       TEXT NOT NULL,
    label       TEXT,
    actor       TEXT,
    created_at  TEXT NOT NULL,
    UNIQUE(task_id, kind, value)
)`,

		`CREATE TABLE specs (
    id              INTEGER PRIMARY KEY,
    uid             TEXT NOT NULL UNIQUE,
    project_id      INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    slug            TEXT NOT NULL,
    title           TEXT,
    source_path     TEXT,
    repo_root       TEXT,
    status          TEXT NOT NULL DEFAULT 'draft'
                    CHECK(status IN ('draft','active','superseded','archived')),
    head_version_id INTEGER REFERENCES spec_versions(id),
    created_at      TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    UNIQUE(project_id, slug)
)`,
		`CREATE TABLE spec_versions (
    id             INTEGER PRIMARY KEY,
    spec_id        INTEGER NOT NULL REFERENCES specs(id) ON DELETE CASCADE,
    n              INTEGER NOT NULL,
    sha256         TEXT NOT NULL,
    git_blob       TEXT,
    git_commit     TEXT,
    body           TEXT NOT NULL,
    front_matter   TEXT,
    parser_version TEXT NOT NULL,
    imported_at    TEXT NOT NULL,
    actor          TEXT,
    UNIQUE(spec_id, n)
)`,
		`CREATE TABLE spec_anchors (
    id                 INTEGER PRIMARY KEY,
    spec_id            INTEGER NOT NULL REFERENCES specs(id) ON DELETE CASCADE,
    key                TEXT NOT NULL,
    kind               TEXT NOT NULL DEFAULT 'heading'
                       CHECK(kind IN ('root','heading','block')),
    state              TEXT NOT NULL DEFAULT 'live'
                       CHECK(state IN ('live','orphaned','retired')),
    pinned_at_version  INTEGER NOT NULL REFERENCES spec_versions(id),
    first_seen_version INTEGER NOT NULL REFERENCES spec_versions(id),
    last_seen_version  INTEGER NOT NULL REFERENCES spec_versions(id),
    created_at         TEXT NOT NULL,
    UNIQUE(spec_id, key)
)`,
		`CREATE TABLE spec_sections (
    id          INTEGER PRIMARY KEY,
    version_id  INTEGER NOT NULL REFERENCES spec_versions(id) ON DELETE CASCADE,
    anchor_id   INTEGER REFERENCES spec_anchors(id) ON DELETE CASCADE,
    parent_id   INTEGER REFERENCES spec_sections(id) ON DELETE CASCADE,
    ordinal     INTEGER NOT NULL,
    level       INTEGER NOT NULL,
    kind        TEXT NOT NULL DEFAULT 'heading'
                CHECK(kind IN ('root','heading','block')),
    number      TEXT,
    heading     TEXT,
    path        TEXT NOT NULL,
    path_slug   TEXT NOT NULL,
    body        TEXT NOT NULL,
    body_sha    TEXT NOT NULL,
    context_sha TEXT NOT NULL,
    line_start  INTEGER NOT NULL,
    line_end    INTEGER NOT NULL,
    byte_start  INTEGER NOT NULL,
    byte_end    INTEGER NOT NULL,
    has_mermaid INTEGER NOT NULL DEFAULT 0,
    coverage_required INTEGER CHECK(coverage_required IN (0,1)),
    match       TEXT NOT NULL DEFAULT 'new'
                CHECK(match IN ('pinned','anchored','path','slug','similar','new')),
    confidence  REAL,
    UNIQUE(version_id, anchor_id),
    UNIQUE(version_id, path_slug),
    UNIQUE(version_id, ordinal)
)`,
		`CREATE INDEX idx_spec_sections_anchor ON spec_sections(anchor_id)`,
		`CREATE INDEX idx_spec_sections_slug ON spec_sections(version_id, path_slug)`,
		`CREATE TABLE spec_anchor_links (
    from_anchor INTEGER NOT NULL REFERENCES spec_anchors(id) ON DELETE CASCADE,
    to_anchor   INTEGER NOT NULL REFERENCES spec_anchors(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL
                CHECK(kind IN ('renamed','moved','split','merged','supersedes')),
    version_id  INTEGER REFERENCES spec_versions(id),
    actor       TEXT,
    created_at  TEXT NOT NULL,
    PRIMARY KEY (from_anchor, to_anchor, kind)
)`,
		`CREATE TABLE spec_task_links (
    id                INTEGER PRIMARY KEY,
    anchor_id         INTEGER NOT NULL REFERENCES spec_anchors(id) ON DELETE CASCADE,
    task_id           INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    subtask_id        INTEGER REFERENCES subtasks(id) ON DELETE CASCADE,
    kind              TEXT NOT NULL DEFAULT 'implements'
                      CHECK(kind IN ('implements','verifies','documents','blocked-by')),
    scope             TEXT NOT NULL DEFAULT 'self'
                      CHECK(scope IN ('self','subtree')),
    agreed_version_id INTEGER REFERENCES spec_versions(id),
    agreed_body_sha   TEXT,
    actor             TEXT,
    created_at        TEXT NOT NULL,
    UNIQUE(anchor_id, task_id, kind, scope)
)`,
		`CREATE INDEX idx_spec_task_links_task ON spec_task_links(task_id)`,
		`CREATE INDEX idx_spec_task_links_reverse ON spec_task_links(anchor_id, task_id)`,
		// A pending import whose reconciliation needs a decision. Durable, so
		// losing the shell does not lose the reconciliation (design §5.4).
		`CREATE TABLE spec_pending (
    id          INTEGER PRIMARY KEY,
    spec_id     INTEGER NOT NULL REFERENCES specs(id) ON DELETE CASCADE,
    base_version_id INTEGER REFERENCES spec_versions(id),
    body        TEXT NOT NULL,
    sha256      TEXT NOT NULL,
    report      TEXT NOT NULL,
    actor       TEXT,
    created_at  TEXT NOT NULL
)`,
		`CREATE VIRTUAL TABLE spec_fts USING fts5(
    heading, body,
    content='spec_sections', content_rowid='id',
    tokenize='unicode61'
)`,
		`CREATE TRIGGER spec_sections_ai AFTER INSERT ON spec_sections BEGIN
    INSERT INTO spec_fts(rowid, heading, body) VALUES (new.id, COALESCE(new.heading, ''), new.body);
END`,
		`CREATE TRIGGER spec_sections_ad AFTER DELETE ON spec_sections BEGIN
    INSERT INTO spec_fts(spec_fts, rowid, heading, body) VALUES ('delete', old.id, COALESCE(old.heading, ''), old.body);
END`,
		`CREATE TRIGGER spec_sections_au AFTER UPDATE ON spec_sections BEGIN
    INSERT INTO spec_fts(spec_fts, rowid, heading, body) VALUES ('delete', old.id, COALESCE(old.heading, ''), old.body);
    INSERT INTO spec_fts(rowid, heading, body) VALUES (new.id, COALESCE(new.heading, ''), new.body);
END`,
		`CREATE INDEX idx_events_id_project ON events(project_id, id)`,
		`INSERT OR IGNORE INTO meta (key, value) VALUES ('db_uid', `+uidExpr+`)`,
	)
	return s
}()

// migration4 is v3 -> v4 (docs/agent-features-spec.md "Schema v4"): session
// identity and claim liveness, handoff acknowledgement, snoozed tasks,
// evidence on done, checkpoints, the actor registry, standing rules and
// shared lessons.
var migration4 = []string{
	`ALTER TABLE events ADD COLUMN session TEXT`,
	`ALTER TABLE tasks ADD COLUMN claim_session TEXT`,
	`ALTER TABLE tasks ADD COLUMN claim_last_seen TEXT`,
	`ALTER TABLE tasks ADD COLUMN snoozed_until TEXT`,
	`ALTER TABLE tasks ADD COLUMN snooze_note TEXT`,
	`ALTER TABLE notes ADD COLUMN acknowledged_at TEXT`,
	`ALTER TABLE notes ADD COLUMN acknowledged_by TEXT`,
	`CREATE INDEX idx_events_session ON events(session)`,
	`CREATE TABLE task_evidence (
    id          INTEGER PRIMARY KEY,
    task_id     INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    subtask_id  INTEGER REFERENCES subtasks(id) ON DELETE CASCADE,
    event_id    INTEGER REFERENCES events(id) ON DELETE SET NULL,
    kind        TEXT NOT NULL
                CHECK(kind IN ('commit','branch','pr','test','file','url','note')),
    value       TEXT NOT NULL,
    actor       TEXT,
    created_at  TEXT NOT NULL
)`,
	`CREATE INDEX idx_task_evidence_task ON task_evidence(task_id)`,
	`CREATE TABLE checkpoints (
    id          INTEGER PRIMARY KEY,
    task_id     INTEGER NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    n           INTEGER NOT NULL,
    body        TEXT NOT NULL,
    next        TEXT,
    actor       TEXT,
    session     TEXT,
    created_at  TEXT NOT NULL,
    UNIQUE(task_id, n)
)`,
	`CREATE TABLE actors (
    id          INTEGER PRIMARY KEY,
    slug        TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL DEFAULT 'agent' CHECK(kind IN ('agent','human')),
    note        TEXT,
    created_at  TEXT NOT NULL
)`,
	`CREATE TABLE actor_aliases (
    alias       TEXT PRIMARY KEY,
    actor_id    INTEGER NOT NULL REFERENCES actors(id) ON DELETE CASCADE
)`,
	`CREATE TABLE rules (
    id          INTEGER PRIMARY KEY,
    uid         TEXT NOT NULL UNIQUE,
    kind        TEXT NOT NULL CHECK(kind IN ('deny','allow','require','remember')),
    verb        TEXT,
    check_name  TEXT,
    text        TEXT,
    scope_type  TEXT NOT NULL CHECK(scope_type IN ('global','context','project','task')),
    scope_id    INTEGER,
    path_glob   TEXT,
    repo        TEXT,
    where_json  TEXT,
    hard        INTEGER NOT NULL DEFAULT 0,
    overridable INTEGER NOT NULL DEFAULT 0,
    status      TEXT NOT NULL DEFAULT 'active'
                CHECK(status IN ('active','proposed','revoked','expired')),
    expires_at  TEXT,
    until_task_closed INTEGER NOT NULL DEFAULT 0,
    session     TEXT,
    source      TEXT NOT NULL DEFAULT 'agent' CHECK(source IN ('user','agent')),
    stated_by   TEXT,
    stated_at   TEXT,
    quote       TEXT,
    supersedes  INTEGER REFERENCES rules(id),
    actor       TEXT,
    created_at  TEXT NOT NULL
)`,
	`CREATE INDEX idx_rules_scope ON rules(scope_type, scope_id, status)`,
	`CREATE TABLE rule_events (
    id          INTEGER PRIMARY KEY,
    rule_id     INTEGER NOT NULL REFERENCES rules(id) ON DELETE CASCADE,
    action      TEXT NOT NULL CHECK(action IN ('add','propose','approve','revoke','expire','amend')),
    actor       TEXT,
    at          TEXT NOT NULL,
    note        TEXT
)`,
	`CREATE TABLE lessons (
    id          INTEGER PRIMARY KEY,
    uid         TEXT NOT NULL UNIQUE,
    title       TEXT NOT NULL,
    body        TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL DEFAULT 'gotcha' CHECK(kind IN ('gotcha','fix','decision','fact')),
    scope_type  TEXT NOT NULL CHECK(scope_type IN ('global','context','project','task')),
    scope_id    INTEGER,
    status      TEXT NOT NULL DEFAULT 'active' CHECK(status IN ('active','retired')),
    source_task INTEGER REFERENCES tasks(id) ON DELETE SET NULL,
    verified_at TEXT,
    verified_by TEXT,
    supersedes  INTEGER REFERENCES lessons(id),
    actor       TEXT,
    created_at  TEXT NOT NULL
)`,
	`CREATE INDEX idx_lessons_scope ON lessons(scope_type, scope_id, status)`,
	`CREATE TABLE lesson_triggers (
    lesson_id   INTEGER NOT NULL REFERENCES lessons(id) ON DELETE CASCADE,
    type        TEXT NOT NULL CHECK(type IN ('path','cmd','topic')),
    value       TEXT NOT NULL,
    PRIMARY KEY (lesson_id, type, value)
)`,
	`CREATE VIRTUAL TABLE lessons_fts USING fts5(
    title, body,
    content='lessons', content_rowid='id',
    tokenize='unicode61'
)`,
	`CREATE TRIGGER lessons_ai AFTER INSERT ON lessons BEGIN
    INSERT INTO lessons_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
END`,
	`CREATE TRIGGER lessons_ad AFTER DELETE ON lessons BEGIN
    INSERT INTO lessons_fts(lessons_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
END`,
	`CREATE TRIGGER lessons_au AFTER UPDATE ON lessons BEGIN
    INSERT INTO lessons_fts(lessons_fts, rowid, title, body) VALUES ('delete', old.id, old.title, old.body);
    INSERT INTO lessons_fts(rowid, title, body) VALUES (new.id, new.title, new.body);
END`,
}
