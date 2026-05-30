// Package clone implements the ports.Workspace contract via full `git clone`
// per session, as an alternative to the gitworktree adapter.
//
// # Semantics vs. gitworktree
//
//   - Create:   git clone <source> <dest>, then git checkout -b <branch>
//     (falls back to `git checkout <branch>` if the branch already exists).
//   - Restore:  if the destination directory exists, verify it is a valid
//     git working tree whose `origin` matches the configured source repo, and
//     reuse it. If the directory does not exist, perform a fresh clone.
//   - Destroy:  validate the path is inside the managed root, refuse to
//     delete if `git status --porcelain` reports uncommitted changes, then
//     `os.RemoveAll` the directory. No `--force` flag is ever passed to git
//     and there is no escape hatch — mirroring the RA fix on gitworktree, the
//     adapter never silently throws away unpushed agent work.
//   - List:     enumerates `<managedRoot>/<projectID>/*` and reports each
//     entry whose current branch is readable via `git branch --show-current`.
//     Corrupt clones are skipped.
//
// # Bare vs. non-bare source
//
// `git clone` works against either a bare repository or a non-bare working
// directory transparently — the source is read-only from git's point of view
// and we never invoke `git -C <source>` for mutating commands. Operators may
// point RepoResolver at either kind.
//
// # Concurrency model
//
// Two Create calls for different (project, session) pairs that share a
// source repo do not contend on any lock file: plain `git clone` reads the
// source's pack files (read-shared on POSIX) and writes only into its own
// destination, so the per-clone `.git/index.lock` is destination-scoped. We
// deliberately do NOT pass `--reference <source>` or `--shared`, which would
// alias the destination's object DB to the source's and create coupled
// failure modes if the source were ever pruned or repacked while a clone
// existed.
//
// The adapter itself is otherwise stateless: no in-memory map of sessions,
// no shared filesystem locks. Concurrent Destroy on the same path is the
// caller's responsibility (the Session Manager already serialises lifecycle
// transitions per session, so this is not an issue in practice).
//
// # Path-escape protection
//
// project / session IDs are rejected if they contain a path separator or
// equal `.` / `..` — see validatePathComponent. Beyond that, every path the
// adapter touches is resolved against the managed root via
// validateManagedPath, which fully expands symlinks (physicalAbs) before
// checking containment. This mirrors the RB fix on PR #23.
package clone

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

const defaultGitBinary = "git"

var (
	// ErrUnsafePath is returned when a path argument escapes the managed root
	// or an id contains path-traversal components.
	ErrUnsafePath = errors.New("clone: unsafe workspace path")

	// ErrDirtyWorkspace is returned by Destroy when the working tree has
	// uncommitted changes. The adapter never deletes dirty workspaces.
	ErrDirtyWorkspace = errors.New("clone: refusing to destroy: workspace has uncommitted changes")

	// ErrOriginMismatch is returned by Restore when an existing directory's
	// origin URL does not match the configured source repository.
	ErrOriginMismatch = errors.New("clone: existing workspace has mismatched origin")
)

// RepoResolver maps a ProjectID to the source repository path/URL passed to
// `git clone`.
type RepoResolver interface {
	RepoPath(projectID domain.ProjectID) (string, error)
}

// StaticRepoResolver is a simple map-backed RepoResolver suitable for tests
// and single-project configurations.
type StaticRepoResolver map[domain.ProjectID]string

func (r StaticRepoResolver) RepoPath(projectID domain.ProjectID) (string, error) {
	path := r[projectID]
	if path == "" {
		return "", fmt.Errorf("clone: no repo configured for project %q", projectID)
	}
	return path, nil
}

// Options configures a clone Workspace. ManagedRoot and RepoResolver are
// required; Binary defaults to "git".
type Options struct {
	Binary       string
	ManagedRoot  string
	RepoResolver RepoResolver
}

// Workspace implements ports.Workspace by cloning the source repo into a
// per-session directory under the managed root.
type Workspace struct {
	binary      string
	managedRoot string
	repos       RepoResolver
	run         commandRunner
}

type commandRunner func(ctx context.Context, binary string, args ...string) ([]byte, error)

var _ ports.Workspace = (*Workspace)(nil)

// New constructs a clone Workspace and resolves the managed root through
// symlinks so subsequent containment checks compare physical paths.
func New(opts Options) (*Workspace, error) {
	binary := opts.Binary
	if binary == "" {
		binary = defaultGitBinary
	}
	if opts.ManagedRoot == "" {
		return nil, errors.New("clone: ManagedRoot is required")
	}
	if opts.RepoResolver == nil {
		return nil, errors.New("clone: RepoResolver is required")
	}
	root, err := physicalAbs(opts.ManagedRoot)
	if err != nil {
		return nil, fmt.Errorf("clone: managed root: %w", err)
	}
	return &Workspace{
		binary:      binary,
		managedRoot: filepath.Clean(root),
		repos:       opts.RepoResolver,
		run:         runCommand,
	}, nil
}

func (w *Workspace) Create(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	if err := validateConfig(cfg); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	repo, err := w.repoPath(cfg.ProjectID)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	path, err := w.managedPath(cfg.ProjectID, cfg.SessionID)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	if exists, err := pathExists(path); err != nil {
		return ports.WorkspaceInfo{}, err
	} else if exists {
		return ports.WorkspaceInfo{}, fmt.Errorf("clone: refusing to create %q: path already exists", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("clone: create parent dir: %w", err)
	}
	if err := w.doClone(ctx, repo, path); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	if err := w.doCheckout(ctx, path, cfg.Branch); err != nil {
		// Cleanup partial clone so the next Create can retry from a clean slate.
		_ = os.RemoveAll(path)
		return ports.WorkspaceInfo{}, err
	}
	return ports.WorkspaceInfo{Path: path, Branch: cfg.Branch, SessionID: cfg.SessionID, ProjectID: cfg.ProjectID}, nil
}

func (w *Workspace) Destroy(ctx context.Context, info ports.WorkspaceInfo) error {
	if info.ProjectID == "" {
		return errors.New("clone: project id is required")
	}
	if info.Path == "" {
		return fmt.Errorf("%w: empty path", ErrUnsafePath)
	}
	path, err := w.validateManagedPath(info.Path)
	if err != nil {
		return err
	}
	exists, err := pathExists(path)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	dirty, err := w.isDirty(ctx, path)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("%w: %q", ErrDirtyWorkspace, path)
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("clone: remove %q: %w", path, err)
	}
	return nil
}

func (w *Workspace) List(ctx context.Context, project domain.ProjectID) ([]ports.WorkspaceInfo, error) {
	if project == "" {
		return nil, errors.New("clone: project id is required")
	}
	if err := validatePathComponent("project id", string(project)); err != nil {
		return nil, err
	}
	projectRoot, err := w.projectRoot(project)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(projectRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("clone: read project dir: %w", err)
	}
	out := make([]ports.WorkspaceInfo, 0, len(entries))
	for _, e := range entries {
		if !isDirEntry(e) {
			continue
		}
		name := e.Name()
		if name == "." || name == ".." {
			continue
		}
		clonePath := filepath.Join(projectRoot, name)
		branch, err := w.currentBranch(ctx, clonePath)
		if err != nil {
			// Corrupt clone — skip it. Mirrors the upstream JS behavior of
			// surfacing only valid sessions in List().
			continue
		}
		out = append(out, ports.WorkspaceInfo{
			Path:      clonePath,
			Branch:    branch,
			SessionID: domain.SessionID(name),
			ProjectID: project,
		})
	}
	return out, nil
}

func (w *Workspace) Restore(ctx context.Context, cfg ports.WorkspaceConfig) (ports.WorkspaceInfo, error) {
	if err := validateConfig(cfg); err != nil {
		return ports.WorkspaceInfo{}, err
	}
	repo, err := w.repoPath(cfg.ProjectID)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	path, err := w.managedPath(cfg.ProjectID, cfg.SessionID)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	exists, err := pathExists(path)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	if !exists {
		// Nothing on disk: fall through to a fresh clone, same as Create.
		return w.Create(ctx, cfg)
	}
	// Path exists: must be a valid git working tree with the expected origin.
	if _, err := w.run(ctx, w.binary, revParseInsideWorkTreeArgs(path)...); err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("clone: refusing to restore %q: not a valid git working tree: %w", path, err)
	}
	expected, err := w.originURL(ctx, repo)
	if err != nil {
		return ports.WorkspaceInfo{}, err
	}
	got, err := w.originURL(ctx, path)
	if err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("clone: read origin of existing workspace: %w", err)
	}
	if !originsEqual(expected, got) {
		return ports.WorkspaceInfo{}, fmt.Errorf("%w: %q origin=%q, expected %q", ErrOriginMismatch, path, got, expected)
	}
	branch, err := w.currentBranch(ctx, path)
	if err != nil {
		return ports.WorkspaceInfo{}, fmt.Errorf("clone: read current branch of %q: %w", path, err)
	}
	if branch == "" {
		branch = cfg.Branch
	}
	return ports.WorkspaceInfo{Path: path, Branch: branch, SessionID: cfg.SessionID, ProjectID: cfg.ProjectID}, nil
}

func (w *Workspace) doClone(ctx context.Context, source, dest string) error {
	if _, err := w.run(ctx, w.binary, cloneArgs(source, dest)...); err != nil {
		// Clean up any partial dest left behind by git.
		_ = os.RemoveAll(dest)
		return fmt.Errorf("clone: git clone %q -> %q: %w", source, dest, err)
	}
	return nil
}

func (w *Workspace) doCheckout(ctx context.Context, dir, branch string) error {
	if _, err := w.run(ctx, w.binary, checkoutNewBranchArgs(dir, branch)...); err == nil {
		return nil
	}
	// Branch may already exist (e.g. it was fetched as a remote-tracking ref);
	// fall back to a plain checkout so Create is idempotent in that case.
	if _, err := w.run(ctx, w.binary, checkoutBranchArgs(dir, branch)...); err != nil {
		return fmt.Errorf("clone: checkout branch %q in %q: %w", branch, dir, err)
	}
	return nil
}

func (w *Workspace) isDirty(ctx context.Context, dir string) (bool, error) {
	out, err := w.run(ctx, w.binary, statusPorcelainArgs(dir)...)
	if err != nil {
		return false, fmt.Errorf("clone: git status %q: %w", dir, err)
	}
	return len(bytes.TrimSpace(out)) > 0, nil
}

func (w *Workspace) originURL(ctx context.Context, dir string) (string, error) {
	out, err := w.run(ctx, w.binary, remoteGetURLArgs(dir)...)
	if err != nil {
		return "", fmt.Errorf("clone: git remote get-url origin in %q: %w", dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func (w *Workspace) currentBranch(ctx context.Context, dir string) (string, error) {
	out, err := w.run(ctx, w.binary, showCurrentBranchArgs(dir)...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (w *Workspace) repoPath(project domain.ProjectID) (string, error) {
	repo, err := w.repos.RepoPath(project)
	if err != nil {
		return "", err
	}
	if repo == "" {
		return "", fmt.Errorf("clone: no repo configured for project %q", project)
	}
	// Local paths are resolved through symlinks for predictable origin
	// comparisons in Restore. URLs (e.g. https://, git@) fall through to
	// physicalAbs's last branch, which returns the input unchanged.
	abs, err := physicalAbs(repo)
	if err != nil {
		return "", fmt.Errorf("clone: repo path: %w", err)
	}
	return abs, nil
}

func (w *Workspace) managedPath(project domain.ProjectID, session domain.SessionID) (string, error) {
	path := filepath.Join(w.managedRoot, string(project), string(session))
	return w.validateManagedPath(path)
}

func (w *Workspace) projectRoot(project domain.ProjectID) (string, error) {
	path := filepath.Join(w.managedRoot, string(project))
	// The project root itself sits one level below managed root and is
	// allowed to be returned as-is; validateManagedPath rejects paths equal
	// to managedRoot but accepts strict descendants like this one.
	return w.validateManagedPath(path)
}

// validateManagedPath ensures path is an absolute, clean, symlink-resolved
// descendant of managedRoot. Mirrors gitworktree's RB fix on PR #23.
func (w *Workspace) validateManagedPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("%w: empty path", ErrUnsafePath)
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: %q is not absolute", ErrUnsafePath, path)
	}
	clean := filepath.Clean(path)
	if clean != path {
		return "", fmt.Errorf("%w: %q is not clean", ErrUnsafePath, path)
	}
	physical, err := physicalAbs(clean)
	if err != nil {
		return "", fmt.Errorf("clone: resolve path %q: %w", path, err)
	}
	clean = physical
	inside, err := pathWithin(w.managedRoot, clean)
	if err != nil {
		return "", err
	}
	if !inside || clean == w.managedRoot {
		return "", fmt.Errorf("%w: %q is outside managed root %q", ErrUnsafePath, clean, w.managedRoot)
	}
	return clean, nil
}

// validateConfig rejects empty fields and ids that could escape the managed
// root once joined into a path. filepath.Join cleans `..` before
// validateManagedPath runs, so a session id of "../other" would otherwise
// resolve back inside managedRoot while breaking per-project isolation —
// same root cause as gitworktree's RB fix on PR #23.
func validateConfig(cfg ports.WorkspaceConfig) error {
	if cfg.ProjectID == "" {
		return errors.New("clone: project id is required")
	}
	if err := validatePathComponent("project id", string(cfg.ProjectID)); err != nil {
		return err
	}
	if cfg.SessionID == "" {
		return errors.New("clone: session id is required")
	}
	if err := validatePathComponent("session id", string(cfg.SessionID)); err != nil {
		return err
	}
	if cfg.Branch == "" {
		return errors.New("clone: branch is required")
	}
	return nil
}

func validatePathComponent(name, value string) error {
	if strings.ContainsAny(value, `/\`) {
		return fmt.Errorf("%w: %s %q must not contain path separators", ErrUnsafePath, name, value)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("%w: %s %q must not be a path-traversal component", ErrUnsafePath, name, value)
	}
	return nil
}

func physicalAbs(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	parent := filepath.Dir(abs)
	base := filepath.Base(abs)
	for parent != "." && parent != string(os.PathSeparator) {
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, base), nil
		}
		base = filepath.Join(filepath.Base(parent), base)
		parent = filepath.Dir(parent)
	}
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(resolved, base), nil
	}
	return abs, nil
}

func pathWithin(root, path string) (bool, error) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false, fmt.Errorf("clone: compare paths: %w", err)
	}
	return rel == "." || (rel != "" && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))), nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("clone: stat %q: %w", path, err)
}

// originsEqual compares two `git remote get-url` outputs. Local paths are
// resolved through symlinks so a managed-root path stored as the origin of
// a clone still matches the same path when the watcher passes it back as a
// fresh string. Scheme-qualified URLs (http://, https://, ssh://, git://)
// bypass path resolution; SCP-style remotes (`user@host:path`) and POSIX
// paths that happen to contain `@` fall through to physicalAbs, where Clean
// preserves the embedded `:` segment so they can't silently collide with a
// distinct local path.
func originsEqual(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == b {
		return true
	}
	if isSchemeURL(a) || isSchemeURL(b) {
		return false
	}
	abs, errA := physicalAbs(a)
	bbs, errB := physicalAbs(b)
	if errA != nil || errB != nil {
		return false
	}
	return abs == bbs
}

// isSchemeURL is intentionally narrower than "contains @" — a POSIX path
// like `/home/user@corp/repo` should still resolve through symlinks for
// equality. SCP-style remotes (`git@host:p`) hit the string-equality branch
// above when both sides match and are correctly classified as non-equal
// against any local path via physicalAbs (which can't produce `git@...:p`).
func isSchemeURL(s string) bool {
	return strings.Contains(s, "://")
}

// isDirEntry reports whether a DirEntry refers to a directory, resolving
// symlinks before deciding (a symlinked directory shows up as non-dir from
// the DirEntry but as dir once its Info is read).
func isDirEntry(e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	info, err := e.Info()
	if err != nil {
		return false
	}
	return info.IsDir()
}

func runCommand(ctx context.Context, binary string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, commandError{args: append([]string{binary}, args...), output: string(out), err: err}
	}
	return out, nil
}

type commandError struct {
	args   []string
	output string
	err    error
}

func (e commandError) Error() string {
	if strings.TrimSpace(e.output) == "" {
		return fmt.Sprintf("%s: %v", strings.Join(e.args, " "), e.err)
	}
	return fmt.Sprintf("%s: %v: %s", strings.Join(e.args, " "), e.err, strings.TrimSpace(e.output))
}

func (e commandError) Unwrap() error { return e.err }
