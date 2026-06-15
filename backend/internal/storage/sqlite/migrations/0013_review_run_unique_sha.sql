-- A partial unique index backstops the per-worker lock in internal/review: it
-- prevents two concurrent (or cross-restart) Trigger calls from recording two
-- review_run rows for the same worker session at the same reviewed commit
-- (issue #242). Rows with an empty target_sha (head not yet observed) are
-- excluded so they aren't blocked — the engine lock still serialises those.

-- +goose Up
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_review_run_session_sha
    ON review_run (session_id, target_sha) WHERE target_sha != '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_review_run_session_sha;
-- +goose StatementEnd
