-- name: UpsertPRComment :exec
INSERT INTO pr_comment (pr_url, comment_id, author, file, line, body, resolved, created_at, thread_id, url, is_bot)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (pr_url, comment_id) DO UPDATE SET
    author = excluded.author, file = excluded.file, line = excluded.line,
    body = excluded.body, resolved = excluded.resolved,
    thread_id = excluded.thread_id, url = excluded.url, is_bot = excluded.is_bot;

-- name: DeletePRComments :exec
DELETE FROM pr_comment WHERE pr_url = ?;

-- name: ListPRComments :many
SELECT pr_url, comment_id, author, file, line, body, resolved, created_at, thread_id, url, is_bot
FROM pr_comment WHERE pr_url = ? ORDER BY created_at, comment_id;
