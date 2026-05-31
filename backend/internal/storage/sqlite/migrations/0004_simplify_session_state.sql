-- +goose Up
-- +goose StatementBegin
DROP TRIGGER IF EXISTS sessions_cdc_update;
DROP TRIGGER IF EXISTS sessions_cdc_insert;

ALTER TABLE sessions ADD COLUMN is_terminated BOOLEAN NOT NULL DEFAULT FALSE;

UPDATE sessions
SET is_terminated = CASE
    WHEN session_state IN ('done', 'terminated') OR termination_reason <> '' THEN 1
    ELSE 0
END;

UPDATE sessions
SET activity_state = 'exited', activity_source = 'runtime'
WHERE is_terminated = 1 AND activity_state NOT IN ('waiting_input', 'blocked');

ALTER TABLE sessions DROP COLUMN session_state;
ALTER TABLE sessions DROP COLUMN termination_reason;
ALTER TABLE sessions DROP COLUMN is_alive;
ALTER TABLE sessions DROP COLUMN detecting_attempts;
ALTER TABLE sessions DROP COLUMN detecting_started_at;
ALTER TABLE sessions DROP COLUMN detecting_evidence_hash;
ALTER TABLE sessions DROP COLUMN runtime_name;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER sessions_cdc_insert
AFTER INSERT ON sessions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_created',
        json_object('id', NEW.id, 'activity', NEW.activity_state, 'isTerminated', NEW.is_terminated),
        NEW.updated_at);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER sessions_cdc_update
AFTER UPDATE ON sessions
WHEN OLD.activity_state <> NEW.activity_state
    OR OLD.is_terminated <> NEW.is_terminated
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_updated',
        json_object('id', NEW.id, 'activity', NEW.activity_state, 'isTerminated', NEW.is_terminated),
        NEW.updated_at);
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS sessions_cdc_update;
DROP TRIGGER IF EXISTS sessions_cdc_insert;

ALTER TABLE sessions ADD COLUMN session_state TEXT NOT NULL DEFAULT 'idle'
    CHECK (session_state IN ('not_started', 'working', 'idle', 'needs_input', 'stuck', 'detecting', 'done', 'terminated'));
ALTER TABLE sessions ADD COLUMN termination_reason TEXT NOT NULL DEFAULT ''
    CHECK (termination_reason IN ('', 'manually_killed', 'runtime_lost', 'agent_process_exited', 'probe_failure', 'error_in_process', 'auto_cleanup', 'pr_merged'));
ALTER TABLE sessions ADD COLUMN is_alive INTEGER NOT NULL DEFAULT 0;
ALTER TABLE sessions ADD COLUMN detecting_attempts INTEGER;
ALTER TABLE sessions ADD COLUMN detecting_started_at TIMESTAMP;
ALTER TABLE sessions ADD COLUMN detecting_evidence_hash TEXT;
ALTER TABLE sessions ADD COLUMN runtime_name TEXT NOT NULL DEFAULT '';

UPDATE sessions
SET session_state = CASE
    WHEN is_terminated = 1 THEN 'terminated'
    WHEN activity_state = 'active' THEN 'working'
    WHEN activity_state = 'waiting_input' THEN 'needs_input'
    WHEN activity_state = 'blocked' THEN 'stuck'
    ELSE 'idle'
END;

ALTER TABLE sessions DROP COLUMN is_terminated;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER sessions_cdc_insert
AFTER INSERT ON sessions
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_created',
        json_object('id', NEW.id, 'state', NEW.session_state, 'terminationReason', NEW.termination_reason,
                    'isAlive', NEW.is_alive, 'activity', NEW.activity_state),
        NEW.updated_at);
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER sessions_cdc_update
AFTER UPDATE ON sessions
WHEN OLD.session_state <> NEW.session_state
    OR OLD.termination_reason <> NEW.termination_reason
    OR OLD.is_alive <> NEW.is_alive
    OR OLD.activity_state <> NEW.activity_state
BEGIN
    INSERT INTO change_log (project_id, session_id, event_type, payload, created_at)
    VALUES (NEW.project_id, NEW.id, 'session_updated',
        json_object('id', NEW.id, 'state', NEW.session_state, 'terminationReason', NEW.termination_reason,
                    'isAlive', NEW.is_alive, 'activity', NEW.activity_state),
        NEW.updated_at);
END;
-- +goose StatementEnd
