-- +goose Up
-- +goose StatementBegin

-- scm_subjects is the durable binding from an AO session to the provider/repo
-- branch and, once discovered, the concrete change request. Provider cache
-- scoping deliberately includes credential_hash; PR identity never does.
CREATE TABLE scm_subjects (
    session_id      TEXT PRIMARY KEY REFERENCES sessions (id) ON DELETE CASCADE,
    project_id      TEXT NOT NULL REFERENCES projects (id),
    provider        TEXT NOT NULL,
    host            TEXT NOT NULL,
    repo            TEXT NOT NULL,
    branch          TEXT NOT NULL,
    base_branch     TEXT NOT NULL DEFAULT '',
    credential_hash TEXT NOT NULL DEFAULT '',
    pr_number       INTEGER NOT NULL DEFAULT 0,
    pr_url          TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMP NOT NULL,
    updated_at      TIMESTAMP NOT NULL
);
CREATE INDEX idx_scm_subjects_project ON scm_subjects (project_id);
CREATE INDEX idx_scm_subjects_repo ON scm_subjects (provider, host, repo, project_id);

-- scm_snapshots is the raw normalized SCM truth. The LCM consumes these through
-- the SCM snapshot event path; the derived pr/pr_checks/pr_comment tables remain
-- the display/reaction read model written by the LCM.
CREATE TABLE scm_snapshots (
    session_id    TEXT NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    revision      INTEGER NOT NULL,
    project_id    TEXT NOT NULL REFERENCES projects (id),
    provider      TEXT NOT NULL,
    host          TEXT NOT NULL,
    repo          TEXT NOT NULL,
    pr_number     INTEGER NOT NULL DEFAULT 0,
    pr_url        TEXT NOT NULL DEFAULT '',
    freshness     TEXT NOT NULL,
    semantic_hash TEXT NOT NULL,
    observed_at   TIMESTAMP NOT NULL,
    snapshot_json TEXT NOT NULL,
    PRIMARY KEY (session_id, revision)
);
CREATE INDEX idx_scm_snapshots_latest ON scm_snapshots (session_id, revision DESC);
CREATE INDEX idx_scm_snapshots_project ON scm_snapshots (project_id, session_id, revision DESC);

-- scm_provider_cache stores provider-owned ETags, positive branch mappings, and
-- opaque provider hints. Generic storage only scopes, expires, and prunes it.
CREATE TABLE scm_provider_cache (
    provider        TEXT NOT NULL,
    host            TEXT NOT NULL,
    repo            TEXT NOT NULL,
    credential_hash TEXT NOT NULL DEFAULT '',
    namespace       TEXT NOT NULL,
    cache_key       TEXT NOT NULL,
    etag            TEXT NOT NULL DEFAULT '',
    value_json      BLOB NOT NULL DEFAULT x'',
    updated_at      TIMESTAMP NOT NULL,
    expires_at      TIMESTAMP,
    PRIMARY KEY (provider, host, repo, credential_hash, namespace, cache_key)
);
CREATE INDEX idx_scm_provider_cache_scope ON scm_provider_cache (
    provider, host, repo, credential_hash, namespace, updated_at
);

-- scm_poll_state is provider/repo scheduler memory: failures, backoff, and
-- rate-limit gates. It is intentionally separate from provider cache because it
-- affects scheduling even when no semantic snapshot changes.
CREATE TABLE scm_poll_state (
    provider             TEXT NOT NULL,
    host                 TEXT NOT NULL,
    repo                 TEXT NOT NULL,
    consecutive_failures INTEGER NOT NULL DEFAULT 0,
    last_success_at      TIMESTAMP,
    last_failure_at      TIMESTAMP,
    backoff_until        TIMESTAMP,
    rate_limit_until     TIMESTAMP,
    last_error_json      TEXT NOT NULL DEFAULT '',
    updated_at           TIMESTAMP NOT NULL,
    PRIMARY KEY (provider, host, repo)
);

-- Preserve SCM review-thread metadata in the derived PR comment read model.
ALTER TABLE pr_comment ADD COLUMN thread_id TEXT NOT NULL DEFAULT '';
ALTER TABLE pr_comment ADD COLUMN url TEXT NOT NULL DEFAULT '';
ALTER TABLE pr_comment ADD COLUMN is_bot INTEGER NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- Snapshot inserts are meaningful semantic changes only: SaveSnapshot suppresses
-- unchanged semantic hashes before writing this table. The trigger turns each
-- inserted revision into the event the SCM consumer uses to feed the LCM.
-- +goose StatementBegin
CREATE TRIGGER scm_snapshots_cdc_insert
AFTER INSERT ON scm_snapshots
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.session_id, 'scm_snapshot_created',
        json_object('sessionId', NEW.session_id, 'revision', NEW.revision,
                    'semanticHash', NEW.semantic_hash, 'provider', NEW.provider,
                    'host', NEW.host, 'repo', NEW.repo, 'prNumber', NEW.pr_number,
                    'prUrl', NEW.pr_url, 'freshness', NEW.freshness),
        NEW.observed_at);
END;
-- +goose StatementEnd

-- Review comments change without necessarily changing scalar PR fields. Emit a
-- small event so API/dashboard live subscribers can refresh the comment read
-- model. The LCM is not driven by these events; it is driven by SCM snapshots.
-- +goose StatementBegin
CREATE TRIGGER pr_comment_cdc_insert
AFTER INSERT ON pr_comment
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_comment_recorded',
        json_object('pr', NEW.pr_url, 'commentId', NEW.comment_id,
                    'threadId', NEW.thread_id, 'author', NEW.author,
                    'isBot', NEW.is_bot, 'resolved', NEW.resolved),
        NEW.created_at);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER pr_comment_cdc_update
AFTER UPDATE ON pr_comment
WHEN OLD.author <> NEW.author
    OR OLD.file <> NEW.file
    OR OLD.line <> NEW.line
    OR OLD.body <> NEW.body
    OR OLD.resolved <> NEW.resolved
    OR OLD.thread_id <> NEW.thread_id
    OR OLD.url <> NEW.url
    OR OLD.is_bot <> NEW.is_bot
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (
        (SELECT s.project_id FROM pr p JOIN sessions s ON s.id = p.session_id WHERE p.url = NEW.pr_url),
        (SELECT session_id FROM pr WHERE url = NEW.pr_url),
        'pr_comment_recorded',
        json_object('pr', NEW.pr_url, 'commentId', NEW.comment_id,
                    'threadId', NEW.thread_id, 'author', NEW.author,
                    'isBot', NEW.is_bot, 'resolved', NEW.resolved),
        NEW.created_at);
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS pr_comment_cdc_update;
DROP TRIGGER IF EXISTS pr_comment_cdc_insert;
DROP TRIGGER IF EXISTS scm_snapshots_cdc_insert;
DROP TABLE IF EXISTS scm_poll_state;
DROP TABLE IF EXISTS scm_provider_cache;
DROP TABLE IF EXISTS scm_snapshots;
DROP TABLE IF EXISTS scm_subjects;
-- SQLite cannot drop columns from pr_comment portably in a down migration here.
-- +goose StatementEnd
