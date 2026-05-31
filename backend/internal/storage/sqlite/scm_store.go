package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

var _ ports.SCMStore = (*Store)(nil)

func (s *Store) UpsertSubject(ctx context.Context, subject domain.SCMSubject) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.upsertSCMSubject(ctx, s.writeDB, subject, s.now())
	return err
}

func (s *Store) GetSubject(ctx context.Context, sessionID domain.SessionID) (domain.SCMSubject, bool, error) {
	row := s.readDB.QueryRowContext(ctx, `
SELECT session_id, project_id, provider, host, repo, branch, base_branch, credential_hash,
       pr_number, pr_url, created_at, updated_at
FROM scm_subjects WHERE session_id = ?`, string(sessionID))
	subj, err := scanSCMSubject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SCMSubject{}, false, nil
	}
	if err != nil {
		return domain.SCMSubject{}, false, fmt.Errorf("get scm subject %s: %w", sessionID, err)
	}
	return subj, true, nil
}

func (s *Store) ListSubjects(ctx context.Context, filter domain.SCMSubjectFilter) ([]domain.SCMSubject, error) {
	where := []string{"1=1"}
	args := []any{}
	if filter.ProjectID != "" {
		where = append(where, "project_id = ?")
		args = append(args, string(filter.ProjectID))
	}
	if filter.Provider != "" {
		where = append(where, "provider = ?")
		args = append(args, string(filter.Provider))
	}
	if strings.TrimSpace(filter.Host) != "" {
		where = append(where, "host = ?")
		args = append(args, normalizeSCMHost(filter.Host))
	}
	if strings.TrimSpace(filter.Repo) != "" {
		where = append(where, "repo = ?")
		args = append(args, normalizeSCMRepo(filter.Repo))
	}
	rows, err := s.readDB.QueryContext(ctx, `
SELECT session_id, project_id, provider, host, repo, branch, base_branch, credential_hash,
       pr_number, pr_url, created_at, updated_at
FROM scm_subjects WHERE `+strings.Join(where, " AND ")+` ORDER BY session_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list scm subjects: %w", err)
	}
	defer rows.Close()
	out := []domain.SCMSubject{}
	for rows.Next() {
		subj, err := scanSCMSubject(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, subj)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) DeleteSubject(ctx context.Context, sessionID domain.SessionID) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.writeDB.ExecContext(ctx, `DELETE FROM scm_subjects WHERE session_id = ?`, string(sessionID))
	return err
}

func (s *Store) SaveSnapshot(ctx context.Context, snapshot domain.SCMSnapshot) (domain.SCMSnapshot, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if snapshot.SessionID == "" {
		snapshot.SessionID = snapshot.Subject.SessionID
	}
	if snapshot.SessionID == "" {
		return domain.SCMSnapshot{}, false, fmt.Errorf("scm store: snapshot missing session id")
	}
	if snapshot.Subject.SessionID == "" {
		snapshot.Subject.SessionID = snapshot.SessionID
	}
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = s.now()
	}
	if snapshot.Freshness == "" {
		snapshot.Freshness = domain.SCMFreshnessFresh
	}
	if snapshot.PR != nil {
		if snapshot.Subject.PRNumber == 0 {
			snapshot.Subject.PRNumber = snapshot.PR.Number
		}
		if snapshot.Subject.PRURL == "" {
			snapshot.Subject.PRURL = snapshot.PR.URL
		}
	}
	if snapshot.Subject.ProjectID == "" {
		if existing, ok, err := s.getSCMSubjectForWrite(ctx, snapshot.SessionID); err != nil {
			return domain.SCMSnapshot{}, false, err
		} else if ok {
			if snapshot.Subject.ProjectID == "" {
				snapshot.Subject.ProjectID = existing.ProjectID
			}
			if snapshot.Subject.Provider == "" {
				snapshot.Subject.Provider = existing.Provider
			}
			if snapshot.Subject.Host == "" {
				snapshot.Subject.Host = existing.Host
			}
			if snapshot.Subject.Repo == "" {
				snapshot.Subject.Repo = existing.Repo
			}
			if snapshot.Subject.Branch == "" {
				snapshot.Subject.Branch = existing.Branch
			}
		}
	}
	hash, err := domain.ComputeSCMSemanticHash(snapshot)
	if err != nil {
		return domain.SCMSnapshot{}, false, err
	}
	snapshot.SemanticHash = hash

	latest, ok, err := s.latestSCMSnapshotForWrite(ctx, snapshot.SessionID)
	if err != nil {
		return domain.SCMSnapshot{}, false, err
	}
	if ok && latest.SemanticHash == snapshot.SemanticHash {
		return latest, false, nil
	}
	if ok {
		snapshot.Revision = latest.Revision + 1
	} else {
		snapshot.Revision = 1
	}
	if snapshot.Subject.CreatedAt.IsZero() {
		if existing, ok, err := s.getSCMSubjectForWrite(ctx, snapshot.SessionID); err != nil {
			return domain.SCMSnapshot{}, false, err
		} else if ok && !existing.CreatedAt.IsZero() {
			snapshot.Subject.CreatedAt = existing.CreatedAt
		} else {
			snapshot.Subject.CreatedAt = snapshot.ObservedAt
		}
	}
	snapshot.Subject.UpdatedAt = snapshot.ObservedAt

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return domain.SCMSnapshot{}, false, fmt.Errorf("begin save scm snapshot: %w", err)
	}
	defer tx.Rollback()

	subj, err := s.upsertSCMSubject(ctx, tx, snapshot.Subject, snapshot.ObservedAt)
	if err != nil {
		return domain.SCMSnapshot{}, false, err
	}
	snapshot.Subject = subj
	if snapshot.Subject.ProjectID == "" {
		return domain.SCMSnapshot{}, false, fmt.Errorf("scm store: snapshot %s missing project id", snapshot.SessionID)
	}
	if snapshot.Subject.Provider == "" || snapshot.Subject.Host == "" || snapshot.Subject.Repo == "" {
		return domain.SCMSnapshot{}, false, fmt.Errorf("scm store: snapshot %s missing provider/repo identity", snapshot.SessionID)
	}
	b, err := json.Marshal(snapshot)
	if err != nil {
		return domain.SCMSnapshot{}, false, err
	}
	prNumber, prURL := snapshot.Subject.PRNumber, snapshot.Subject.PRURL
	if snapshot.PR != nil {
		prNumber = snapshot.PR.Number
		prURL = snapshot.PR.URL
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO scm_snapshots (
    session_id, revision, project_id, provider, host, repo, pr_number, pr_url,
    freshness, semantic_hash, observed_at, snapshot_json
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(snapshot.SessionID), snapshot.Revision, string(snapshot.Subject.ProjectID),
		string(snapshot.Subject.Provider), normalizeSCMHost(snapshot.Subject.Host), normalizeSCMRepo(snapshot.Subject.Repo),
		int64(prNumber), prURL, string(snapshot.Freshness), snapshot.SemanticHash, snapshot.ObservedAt, string(b),
	); err != nil {
		return domain.SCMSnapshot{}, false, fmt.Errorf("insert scm snapshot: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return domain.SCMSnapshot{}, false, fmt.Errorf("commit save scm snapshot: %w", err)
	}
	return snapshot, true, nil
}

func (s *Store) GetLatestSnapshot(ctx context.Context, sessionID domain.SessionID) (domain.SCMSnapshot, bool, error) {
	row := s.readDB.QueryRowContext(ctx, `
SELECT snapshot_json FROM scm_snapshots
WHERE session_id = ?
ORDER BY revision DESC LIMIT 1`, string(sessionID))
	var raw string
	if err := row.Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return domain.SCMSnapshot{}, false, nil
	} else if err != nil {
		return domain.SCMSnapshot{}, false, fmt.Errorf("get latest scm snapshot %s: %w", sessionID, err)
	}
	snap, err := decodeSCMSnapshot(raw)
	if err != nil {
		return domain.SCMSnapshot{}, false, err
	}
	return snap, true, nil
}

func (s *Store) ListLatestSnapshots(ctx context.Context, project domain.ProjectID) ([]domain.SCMSnapshot, error) {
	args := []any{}
	projectWhere := ""
	if project != "" {
		projectWhere = "WHERE ss.project_id = ?"
		args = append(args, string(project))
	}
	rows, err := s.readDB.QueryContext(ctx, `
SELECT ss.snapshot_json
FROM scm_snapshots ss
JOIN (
    SELECT session_id, MAX(revision) AS revision
    FROM scm_snapshots
    GROUP BY session_id
) latest ON latest.session_id = ss.session_id AND latest.revision = ss.revision
`+projectWhere+`
ORDER BY ss.session_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list latest scm snapshots: %w", err)
	}
	defer rows.Close()
	out := []domain.SCMSnapshot{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		snap, err := decodeSCMSnapshot(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) GetProviderCache(ctx context.Context, key domain.SCMProviderCacheKey) (domain.SCMProviderCacheEntry, bool, error) {
	key = normalizeCacheKey(key)
	row := s.readDB.QueryRowContext(ctx, `
SELECT etag, value_json, updated_at, expires_at
FROM scm_provider_cache
WHERE provider = ? AND host = ? AND repo = ? AND credential_hash = ? AND namespace = ? AND cache_key = ?`,
		string(key.Provider), normalizeSCMHost(key.Host), normalizeSCMRepo(key.Repo), key.CredentialHash, key.Namespace, key.Key)
	var entry domain.SCMProviderCacheEntry
	var expires sql.NullTime
	entry.Key = key
	if err := row.Scan(&entry.ETag, &entry.Value, &entry.UpdatedAt, &expires); errors.Is(err, sql.ErrNoRows) {
		return domain.SCMProviderCacheEntry{}, false, nil
	} else if err != nil {
		return domain.SCMProviderCacheEntry{}, false, fmt.Errorf("get scm provider cache %s: %w", key.String(), err)
	}
	if expires.Valid {
		entry.ExpiresAt = expires.Time
	}
	if entry.Expired(s.now()) {
		_ = s.DeleteProviderCache(ctx, domain.SCMProviderCachePrefix{SCMProviderCacheScope: key.SCMProviderCacheScope, Namespace: key.Namespace, KeyPrefix: key.Key})
		return domain.SCMProviderCacheEntry{}, false, nil
	}
	return entry, true, nil
}

func (s *Store) PutProviderCache(ctx context.Context, entry domain.SCMProviderCacheEntry) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	entry.Key = normalizeCacheKey(entry.Key)
	if entry.UpdatedAt.IsZero() {
		entry.UpdatedAt = s.now()
	}
	if _, err := s.writeDB.ExecContext(ctx, `
INSERT INTO scm_provider_cache (
    provider, host, repo, credential_hash, namespace, cache_key, etag, value_json, updated_at, expires_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (provider, host, repo, credential_hash, namespace, cache_key) DO UPDATE SET
    etag = excluded.etag,
    value_json = excluded.value_json,
    updated_at = excluded.updated_at,
    expires_at = excluded.expires_at`,
		string(entry.Key.Provider), normalizeSCMHost(entry.Key.Host), normalizeSCMRepo(entry.Key.Repo),
		entry.Key.CredentialHash, entry.Key.Namespace, entry.Key.Key, entry.ETag, []byte(entry.Value),
		entry.UpdatedAt, nullTime(entry.ExpiresAt),
	); err != nil {
		return fmt.Errorf("put scm provider cache %s: %w", entry.Key.String(), err)
	}
	if entry.MaxEntries > 0 {
		if _, err := s.writeDB.ExecContext(ctx, `
DELETE FROM scm_provider_cache
WHERE rowid IN (
    SELECT rowid FROM scm_provider_cache
    WHERE provider = ? AND host = ? AND repo = ? AND credential_hash = ? AND namespace = ?
    ORDER BY updated_at DESC
    LIMIT -1 OFFSET ?
)`,
			string(entry.Key.Provider), normalizeSCMHost(entry.Key.Host), normalizeSCMRepo(entry.Key.Repo),
			entry.Key.CredentialHash, entry.Key.Namespace, entry.MaxEntries,
		); err != nil {
			return fmt.Errorf("prune scm provider cache %s: %w", entry.Key.String(), err)
		}
	}
	return nil
}

func (s *Store) DeleteProviderCache(ctx context.Context, prefix domain.SCMProviderCachePrefix) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	where, args := providerCachePrefixWhere(prefix)
	_, err := s.writeDB.ExecContext(ctx, `DELETE FROM scm_provider_cache WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("delete scm provider cache: %w", err)
	}
	return nil
}

func (s *Store) GetPollState(ctx context.Context, key domain.SCMPollStateKey) (domain.SCMPollState, bool, error) {
	key = normalizePollStateKey(key)
	row := s.readDB.QueryRowContext(ctx, `
SELECT consecutive_failures, last_success_at, last_failure_at, backoff_until, rate_limit_until, last_error_json
FROM scm_poll_state
WHERE provider = ? AND host = ? AND repo = ?`, string(key.Provider), normalizeSCMHost(key.Host), normalizeSCMRepo(key.Repo))
	st := domain.SCMPollState{Key: key}
	var lastSuccess, lastFailure, backoff, rateLimit sql.NullTime
	var lastErr string
	if err := row.Scan(&st.ConsecutiveFail, &lastSuccess, &lastFailure, &backoff, &rateLimit, &lastErr); errors.Is(err, sql.ErrNoRows) {
		return domain.SCMPollState{}, false, nil
	} else if err != nil {
		return domain.SCMPollState{}, false, fmt.Errorf("get scm poll state %s: %w", key.String(), err)
	}
	if lastSuccess.Valid {
		st.LastSuccessAt = lastSuccess.Time
	}
	if lastFailure.Valid {
		st.LastFailureAt = lastFailure.Time
	}
	if backoff.Valid {
		st.BackoffUntil = backoff.Time
	}
	if rateLimit.Valid {
		st.RateLimitUntil = rateLimit.Time
	}
	if strings.TrimSpace(lastErr) != "" {
		var se domain.SCMError
		if err := json.Unmarshal([]byte(lastErr), &se); err != nil {
			return domain.SCMPollState{}, false, fmt.Errorf("decode scm poll state error %s: %w", key.String(), err)
		}
		st.LastError = &se
	}
	return st, true, nil
}

func (s *Store) PutPollState(ctx context.Context, state domain.SCMPollState) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	state.Key = normalizePollStateKey(state.Key)
	var lastErr string
	if state.LastError != nil {
		b, err := json.Marshal(state.LastError)
		if err != nil {
			return err
		}
		lastErr = string(b)
	}
	_, err := s.writeDB.ExecContext(ctx, `
INSERT INTO scm_poll_state (
    provider, host, repo, consecutive_failures, last_success_at, last_failure_at,
    backoff_until, rate_limit_until, last_error_json, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (provider, host, repo) DO UPDATE SET
    consecutive_failures = excluded.consecutive_failures,
    last_success_at = excluded.last_success_at,
    last_failure_at = excluded.last_failure_at,
    backoff_until = excluded.backoff_until,
    rate_limit_until = excluded.rate_limit_until,
    last_error_json = excluded.last_error_json,
    updated_at = excluded.updated_at`,
		string(state.Key.Provider), normalizeSCMHost(state.Key.Host), normalizeSCMRepo(state.Key.Repo),
		int64(state.ConsecutiveFail), nullTime(state.LastSuccessAt), nullTime(state.LastFailureAt),
		nullTime(state.BackoffUntil), nullTime(state.RateLimitUntil), lastErr, s.now(),
	)
	if err != nil {
		return fmt.Errorf("put scm poll state %s: %w", state.Key.String(), err)
	}
	return nil
}

type scanner interface {
	Scan(dest ...any) error
}

type execer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s *Store) upsertSCMSubject(ctx context.Context, db execer, subject domain.SCMSubject, now time.Time) (domain.SCMSubject, error) {
	if subject.SessionID == "" {
		return domain.SCMSubject{}, fmt.Errorf("scm subject missing session id")
	}
	if subject.ProjectID == "" {
		return domain.SCMSubject{}, fmt.Errorf("scm subject %s missing project id", subject.SessionID)
	}
	if subject.Provider == "" || subject.Host == "" || subject.Repo == "" || subject.Branch == "" {
		return domain.SCMSubject{}, fmt.Errorf("scm subject %s missing provider/repo/branch", subject.SessionID)
	}
	if subject.CreatedAt.IsZero() {
		if existing, ok, err := s.getSCMSubjectForWrite(ctx, subject.SessionID); err != nil {
			return domain.SCMSubject{}, err
		} else if ok && !existing.CreatedAt.IsZero() {
			subject.CreatedAt = existing.CreatedAt
		} else {
			subject.CreatedAt = now
		}
	}
	subject.UpdatedAt = now
	subject.Host = normalizeSCMHost(subject.Host)
	subject.Repo = normalizeSCMRepo(subject.Repo)
	_, err := db.ExecContext(ctx, `
INSERT INTO scm_subjects (
    session_id, project_id, provider, host, repo, branch, base_branch, credential_hash,
    pr_number, pr_url, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (session_id) DO UPDATE SET
    project_id = excluded.project_id,
    provider = excluded.provider,
    host = excluded.host,
    repo = excluded.repo,
    branch = excluded.branch,
    base_branch = excluded.base_branch,
    credential_hash = excluded.credential_hash,
    pr_number = excluded.pr_number,
    pr_url = excluded.pr_url,
    updated_at = excluded.updated_at`,
		string(subject.SessionID), string(subject.ProjectID), string(subject.Provider), subject.Host, subject.Repo,
		subject.Branch, subject.BaseBranch, subject.CredentialHash, int64(subject.PRNumber), subject.PRURL,
		subject.CreatedAt, subject.UpdatedAt,
	)
	if err != nil {
		return domain.SCMSubject{}, fmt.Errorf("upsert scm subject %s: %w", subject.SessionID, err)
	}
	return subject, nil
}

func (s *Store) getSCMSubjectForWrite(ctx context.Context, sessionID domain.SessionID) (domain.SCMSubject, bool, error) {
	row := s.writeDB.QueryRowContext(ctx, `
SELECT session_id, project_id, provider, host, repo, branch, base_branch, credential_hash,
       pr_number, pr_url, created_at, updated_at
FROM scm_subjects WHERE session_id = ?`, string(sessionID))
	subj, err := scanSCMSubject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.SCMSubject{}, false, nil
	}
	if err != nil {
		return domain.SCMSubject{}, false, fmt.Errorf("get scm subject for write %s: %w", sessionID, err)
	}
	return subj, true, nil
}

func (s *Store) latestSCMSnapshotForWrite(ctx context.Context, sessionID domain.SessionID) (domain.SCMSnapshot, bool, error) {
	row := s.writeDB.QueryRowContext(ctx, `
SELECT snapshot_json FROM scm_snapshots
WHERE session_id = ?
ORDER BY revision DESC LIMIT 1`, string(sessionID))
	var raw string
	if err := row.Scan(&raw); errors.Is(err, sql.ErrNoRows) {
		return domain.SCMSnapshot{}, false, nil
	} else if err != nil {
		return domain.SCMSnapshot{}, false, fmt.Errorf("get latest scm snapshot for write %s: %w", sessionID, err)
	}
	snap, err := decodeSCMSnapshot(raw)
	if err != nil {
		return domain.SCMSnapshot{}, false, err
	}
	return snap, true, nil
}

func scanSCMSubject(row scanner) (domain.SCMSubject, error) {
	var subj domain.SCMSubject
	var provider string
	var prNumber int64
	if err := row.Scan(
		&subj.SessionID,
		&subj.ProjectID,
		&provider,
		&subj.Host,
		&subj.Repo,
		&subj.Branch,
		&subj.BaseBranch,
		&subj.CredentialHash,
		&prNumber,
		&subj.PRURL,
		&subj.CreatedAt,
		&subj.UpdatedAt,
	); err != nil {
		return domain.SCMSubject{}, err
	}
	subj.Provider = domain.SCMProvider(provider)
	subj.PRNumber = int(prNumber)
	return subj, nil
}

func decodeSCMSnapshot(raw string) (domain.SCMSnapshot, error) {
	var snap domain.SCMSnapshot
	if err := json.Unmarshal([]byte(raw), &snap); err != nil {
		return domain.SCMSnapshot{}, fmt.Errorf("decode scm snapshot: %w", err)
	}
	return snap, nil
}

func providerCachePrefixWhere(prefix domain.SCMProviderCachePrefix) (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if prefix.Provider != "" {
		where = append(where, "provider = ?")
		args = append(args, string(prefix.Provider))
	}
	if strings.TrimSpace(prefix.Host) != "" {
		where = append(where, "host = ?")
		args = append(args, normalizeSCMHost(prefix.Host))
	}
	if strings.TrimSpace(prefix.Repo) != "" {
		where = append(where, "repo = ?")
		args = append(args, normalizeSCMRepo(prefix.Repo))
	}
	if prefix.CredentialHash != "" {
		where = append(where, "credential_hash = ?")
		args = append(args, prefix.CredentialHash)
	}
	if prefix.Namespace != "" {
		where = append(where, "namespace = ?")
		args = append(args, prefix.Namespace)
	}
	if prefix.KeyPrefix != "" {
		where = append(where, "cache_key LIKE ? ESCAPE '\\'")
		args = append(args, escapeLike(prefix.KeyPrefix)+"%")
	}
	return strings.Join(where, " AND "), args
}

func normalizeCacheKey(key domain.SCMProviderCacheKey) domain.SCMProviderCacheKey {
	key.Host = normalizeSCMHost(key.Host)
	key.Repo = normalizeSCMRepo(key.Repo)
	return key
}

func normalizePollStateKey(key domain.SCMPollStateKey) domain.SCMPollStateKey {
	key.Host = normalizeSCMHost(key.Host)
	key.Repo = normalizeSCMRepo(key.Repo)
	return key
}

func normalizeSCMHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	return strings.TrimSuffix(host, "/")
}

func normalizeSCMRepo(repo string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(repo), "/"))
}

func escapeLike(s string) string {
	// Existing cache keys only use exact prefixes from providers, but keep a
	// conservative escape for SQL LIKE wildcards.
	repl := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return repl.Replace(s)
}

func (s *Store) now() time.Time { return time.Now() }
