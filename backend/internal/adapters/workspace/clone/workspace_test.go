package clone

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

// TestCommandArgs is the clone analogue of gitworktree's same-named test:
// pin the exact argv each git invocation produces so future refactors of
// workspace.go can't silently change the wire format.
func TestCommandArgs(t *testing.T) {
	dir := "/managed/proj/sess"
	cases := []struct {
		name string
		got  []string
		want []string
	}{
		{"remote get-url", remoteGetURLArgs("/repo"), []string{"-C", "/repo", "remote", "get-url", "origin"}},
		{"clone", cloneArgs("/repo", dir), []string{"clone", "/repo", dir}},
		{"checkout new branch", checkoutNewBranchArgs(dir, "feature/x"), []string{"-C", dir, "checkout", "-b", "feature/x"}},
		{"checkout existing branch", checkoutBranchArgs(dir, "feature/x"), []string{"-C", dir, "checkout", "feature/x"}},
		{"show current branch", showCurrentBranchArgs(dir), []string{"-C", dir, "branch", "--show-current"}},
		{"status porcelain", statusPorcelainArgs(dir), []string{"-C", dir, "status", "--porcelain"}},
		{"rev-parse is-inside-work-tree", revParseInsideWorkTreeArgs(dir), []string{"-C", dir, "rev-parse", "--is-inside-work-tree"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !reflect.DeepEqual(tc.got, tc.want) {
				t.Fatalf("args = %#v, want %#v", tc.got, tc.want)
			}
		})
	}
}

// TestCommandArgsNeverUseForce is a belt-and-braces check that no arg
// builder in this package ever emits a `--force` or `-f` flag. The clone
// adapter's safety story rests on never asking git to destroy uncommitted
// work — see Destroy / statusPorcelainArgs doc comment. This catches a
// regression where a future builder might add one.
func TestCommandArgsNeverUseForce(t *testing.T) {
	all := [][]string{
		remoteGetURLArgs("/r"),
		cloneArgs("/r", "/d"),
		checkoutNewBranchArgs("/d", "b"),
		checkoutBranchArgs("/d", "b"),
		showCurrentBranchArgs("/d"),
		statusPorcelainArgs("/d"),
		revParseInsideWorkTreeArgs("/d"),
	}
	for _, args := range all {
		for _, a := range args {
			if a == "--force" || a == "-f" {
				t.Fatalf("arg list %v contains forbidden flag %q", args, a)
			}
		}
	}
}

// TestValidateConfigRejectsPathEscapingIDs is the clone analogue of
// gitworktree's RB-fix test on PR #23. filepath.Join cleans `..` before
// validateManagedPath sees the result, so ids carrying separators or
// `.`/`..` must be rejected at the source.
func TestValidateConfigRejectsPathEscapingIDs(t *testing.T) {
	root := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": root}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	cases := []struct {
		name string
		cfg  ports.WorkspaceConfig
	}{
		{"session contains slash escapes project root", ports.WorkspaceConfig{ProjectID: "proj", SessionID: "../other", Branch: "main"}},
		{"session is ..", ports.WorkspaceConfig{ProjectID: "proj", SessionID: "..", Branch: "main"}},
		{"session is .", ports.WorkspaceConfig{ProjectID: "proj", SessionID: ".", Branch: "main"}},
		{"session contains backslash", ports.WorkspaceConfig{ProjectID: "proj", SessionID: `evil\sess`, Branch: "main"}},
		{"project contains slash escapes managed root", ports.WorkspaceConfig{ProjectID: "../proj", SessionID: "sess", Branch: "main"}},
		{"project is ..", ports.WorkspaceConfig{ProjectID: "..", SessionID: "sess", Branch: "main"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ws.Create(context.Background(), tc.cfg); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("Create err = %v, want ErrUnsafePath", err)
			}
			if _, err := ws.Restore(context.Background(), tc.cfg); !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("Restore err = %v, want ErrUnsafePath", err)
			}
		})
	}
}

func TestValidateConfigAcceptsBenignIDs(t *testing.T) {
	cases := []ports.WorkspaceConfig{
		{ProjectID: "proj-1", SessionID: "sess_2", Branch: "main"},
		{ProjectID: "foo.bar", SessionID: "abc-42", Branch: "main"},
		{ProjectID: "p", SessionID: "..hidden", Branch: "main"}, // leading dots != ".."
	}
	for i, cfg := range cases {
		if err := validateConfig(cfg); err != nil {
			t.Errorf("case %d %+v: unexpected error: %v", i, cfg, err)
		}
	}
}

func TestManagedPathSafety(t *testing.T) {
	root := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": root}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	path, err := ws.managedPath("proj", "sess")
	if err != nil {
		t.Fatalf("managed path: %v", err)
	}
	if want := filepath.Join(ws.managedRoot, "proj", "sess"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if _, err := ws.validateManagedPath(filepath.Join(root, "..", "outside")); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("outside error = %v, want ErrUnsafePath", err)
	}
	if _, err := ws.validateManagedPath("relative/path"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("relative error = %v, want ErrUnsafePath", err)
	}
}

// scriptedRunner records every git invocation and dispatches to a handler
// keyed by the joined argv. Tests build one of these per case to assert on
// the exact sequence of git calls. Mirrors the inline `ws.run = func(...)`
// pattern in gitworktree, but factored so each test case is data-only.
type scriptedRunner struct {
	t        *testing.T
	handlers []scriptedCase
	calls    [][]string
}

type scriptedCase struct {
	match  string // substring of "binary args..."; first matching handler wins
	stdout string
	err    error
}

func (s *scriptedRunner) run(_ context.Context, binary string, args ...string) ([]byte, error) {
	joined := binary + " " + strings.Join(args, " ")
	s.calls = append(s.calls, append([]string{binary}, args...))
	for _, h := range s.handlers {
		if strings.Contains(joined, h.match) {
			return []byte(h.stdout), h.err
		}
	}
	s.t.Fatalf("scriptedRunner: no handler for %q", joined)
	return nil, nil
}

func (s *scriptedRunner) callsMatching(substr string) int {
	n := 0
	for _, c := range s.calls {
		if strings.Contains(strings.Join(c, " "), substr) {
			n++
		}
	}
	return n
}

// resolved is a test helper that runs filepath.EvalSymlinks so test
// assertions can match the canonical paths the adapter sees. On macOS
// t.TempDir() returns paths under /var/folders/... which physicalAbs
// resolves to /private/var/folders/...; without this helper match strings
// would never align.
func resolved(t *testing.T, p string) string {
	t.Helper()
	got, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("eval symlinks %q: %v", p, err)
	}
	return got
}

// TestCreate covers both the happy path and the branch-already-exists
// fallback, in table form. Programmable runner only — no real git.
func TestCreate(t *testing.T) {
	cases := []struct {
		name       string
		branch     string
		handlers   []scriptedCase
		wantBranch string
		wantErr    string
	}{
		{
			name:   "fresh branch via checkout -b",
			branch: "feat/new",
			handlers: []scriptedCase{
				{match: "clone ", stdout: "", err: nil},
				{match: "checkout -b feat/new", stdout: "", err: nil},
			},
			wantBranch: "feat/new",
		},
		{
			name:   "existing branch falls back to plain checkout",
			branch: "feat/existing",
			handlers: []scriptedCase{
				{match: "clone ", stdout: "", err: nil},
				{match: "checkout -b feat/existing", stdout: "fatal: A branch named 'feat/existing' already exists", err: errors.New("exit 128")},
				{match: "checkout feat/existing", stdout: "", err: nil},
			},
			wantBranch: "feat/existing",
		},
		{
			name:   "clone failure surfaces error and cleans dest",
			branch: "feat/x",
			handlers: []scriptedCase{
				{match: "clone ", stdout: "fatal: not a repo", err: errors.New("exit 128")},
			},
			wantErr: "git clone",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			repo := t.TempDir()
			ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			runner := &scriptedRunner{t: t, handlers: tc.handlers}
			ws.run = runner.run

			info, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess", Branch: tc.branch})
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantErr)
				}
				// Partial-clone cleanup must run: no surviving directory.
				if _, statErr := os.Stat(filepath.Join(ws.managedRoot, "proj", "sess")); !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("dest not cleaned up after clone failure: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			wantPath := filepath.Join(ws.managedRoot, "proj", "sess")
			if info.Path != wantPath || info.Branch != tc.wantBranch || info.SessionID != "sess" || info.ProjectID != "proj" {
				t.Fatalf("info = %#v, wantPath=%q wantBranch=%q", info, wantPath, tc.wantBranch)
			}
			if runner.callsMatching("clone "+resolved(t, repo)) == 0 {
				t.Fatalf("expected clone call against repo %q, calls=%v", resolved(t, repo), runner.calls)
			}
		})
	}
}

func TestCreateRefusesExistingDest(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	dest := filepath.Join(ws.managedRoot, "proj", "sess")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("seed dest: %v", err)
	}
	ws.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatalf("git should not be invoked when dest exists")
		return nil, nil
	}
	if _, err := ws.Create(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess", Branch: "main"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("create err = %v, want already-exists refusal", err)
	}
}

// TestRestoreReusesValidClone seeds an on-disk directory and lets the
// scripted runner agree that origin matches the configured repo. Expected
// behavior: Restore returns the existing path with the live current branch.
func TestRestoreReusesValidClone(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	dest := filepath.Join(ws.managedRoot, "proj", "sess")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	originURL := "https://github.com/test/repo.git"
	repoResolved := resolved(t, repo)
	ws.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "rev-parse --is-inside-work-tree"):
			return []byte("true\n"), nil
		case strings.Contains(joined, "-C "+repoResolved+" remote get-url origin"):
			return []byte(originURL + "\n"), nil
		case strings.Contains(joined, "-C "+dest+" remote get-url origin"):
			return []byte(originURL + "\n"), nil
		case strings.Contains(joined, "branch --show-current"):
			return []byte("feature/restored\n"), nil
		}
		t.Fatalf("unexpected git call: %s", joined)
		return nil, nil
	}
	info, err := ws.Restore(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess", Branch: "feature/wanted"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if info.Path != dest || info.Branch != "feature/restored" || info.SessionID != "sess" || info.ProjectID != "proj" {
		t.Fatalf("info = %#v", info)
	}
}

// TestRestoreRejectsMismatchedOrigin guards against silently reusing a
// directory that contains a clone of some other repository — the Session
// Manager would otherwise hand the agent a workspace pointing at the wrong
// upstream, and a push would land in a different repo than expected.
func TestRestoreRejectsMismatchedOrigin(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	dest := filepath.Join(ws.managedRoot, "proj", "sess")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatalf("seed: %v", err)
	}
	repoResolved := resolved(t, repo)
	ws.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "rev-parse --is-inside-work-tree"):
			return []byte("true\n"), nil
		case strings.Contains(joined, "-C "+repoResolved+" remote get-url origin"):
			return []byte("https://github.com/expected/repo.git\n"), nil
		case strings.Contains(joined, "-C "+dest+" remote get-url origin"):
			return []byte("https://github.com/wrong/repo.git\n"), nil
		}
		t.Fatalf("unexpected git call: %s", joined)
		return nil, nil
	}
	_, err = ws.Restore(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess", Branch: "main"})
	if !errors.Is(err, ErrOriginMismatch) {
		t.Fatalf("restore err = %v, want ErrOriginMismatch", err)
	}
}

// TestRestoreReclonesMissingPath: if nothing is on disk we delegate to
// Create. We assert the canonical Create call chain runs (clone + checkout).
func TestRestoreReclonesMissingPath(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	runner := &scriptedRunner{t: t, handlers: []scriptedCase{
		{match: "clone ", stdout: "", err: nil},
		{match: "checkout -b main", stdout: "", err: nil},
	}}
	ws.run = runner.run
	info, err := ws.Restore(context.Background(), ports.WorkspaceConfig{ProjectID: "proj", SessionID: "sess", Branch: "main"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if info.Branch != "main" {
		t.Fatalf("info.Branch = %q, want main", info.Branch)
	}
	if runner.callsMatching("clone") == 0 || runner.callsMatching("checkout -b main") == 0 {
		t.Fatalf("expected clone + checkout calls, got %v", runner.calls)
	}
}

// TestDestroySucceedsForCleanRepo: managed path, clean status, directory
// goes away. This is the happy-path inverse of the dirty-refusal test below.
func TestDestroySucceedsForCleanRepo(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	dest := filepath.Join(ws.managedRoot, "proj", "sess")
	if err := mkdirFile(dest, "keep.txt"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ws.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "status --porcelain") {
			return []byte(""), nil
		}
		t.Fatalf("unexpected git call: %s", joined)
		return nil, nil
	}
	if err := ws.Destroy(context.Background(), ports.WorkspaceInfo{ProjectID: "proj", SessionID: "sess", Path: dest, Branch: "main"}); err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if _, statErr := os.Stat(dest); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("destroy left path on disk: %v", statErr)
	}
}

// TestDestroyRefusesDirtyRepo: with uncommitted changes Destroy must
// preserve the directory. This is the clone analogue of gitworktree's
// RA-fix test on PR #23.
func TestDestroyRefusesDirtyRepo(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	dest := filepath.Join(ws.managedRoot, "proj", "sess")
	if err := mkdirFile(dest, "keep.txt"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	ws.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "status --porcelain") {
			// Simulate a dirty working tree.
			return []byte(" M src/foo.go\n?? new.txt\n"), nil
		}
		t.Fatalf("unexpected git call (Destroy must never call git beyond status): %s", joined)
		return nil, nil
	}
	err = ws.Destroy(context.Background(), ports.WorkspaceInfo{ProjectID: "proj", SessionID: "sess", Path: dest, Branch: "main"})
	if !errors.Is(err, ErrDirtyWorkspace) {
		t.Fatalf("destroy err = %v, want ErrDirtyWorkspace", err)
	}
	if _, statErr := os.Stat(filepath.Join(dest, "keep.txt")); statErr != nil {
		t.Fatalf("destroy deleted dirty workspace: %v", statErr)
	}
}

// TestDestroyRejectsPathEscape covers all three known shapes of path-escape
// attempt in a single table: relative `..` segments, absolute paths outside
// the managed root, and a symlink-ladder that points outside the root.
func TestDestroyRejectsPathEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": root}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	// Build a symlink under managedRoot that points to a sibling outside.
	symlinkParent := filepath.Join(ws.managedRoot, "proj")
	if err := os.MkdirAll(symlinkParent, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	symlinkPath := filepath.Join(symlinkParent, "ladder")
	if err := os.Symlink(outside, symlinkPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	cases := []struct {
		name string
		path string
	}{
		{"unclean dotdot segment", filepath.Join(ws.managedRoot, "..", "outside")},
		{"absolute path outside root", outside},
		{"symlink ladder out of root", symlinkPath},
		{"empty path", ""},
		{"relative path", "relative/sess"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws.run = func(context.Context, string, ...string) ([]byte, error) {
				t.Fatalf("git should never be invoked for path-escape attempt")
				return nil, nil
			}
			err := ws.Destroy(context.Background(), ports.WorkspaceInfo{ProjectID: "proj", SessionID: "sess", Path: tc.path, Branch: "main"})
			if !errors.Is(err, ErrUnsafePath) {
				t.Fatalf("destroy %q err = %v, want ErrUnsafePath", tc.path, err)
			}
		})
	}
}

// TestDestroyMissingPathIsNoop: Restore can call Destroy as part of
// recovery; an absent directory should not be an error.
func TestDestroyMissingPathIsNoop(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	ws.run = func(context.Context, string, ...string) ([]byte, error) {
		t.Fatalf("git should not be invoked for a non-existent path")
		return nil, nil
	}
	dest := filepath.Join(ws.managedRoot, "proj", "missing")
	if err := ws.Destroy(context.Background(), ports.WorkspaceInfo{ProjectID: "proj", SessionID: "missing", Path: dest, Branch: "main"}); err != nil {
		t.Fatalf("destroy missing: %v", err)
	}
}

// TestList exercises the two non-trivial branches of List: missing project
// directory returns empty, and a directory full of mixed entries gets
// filtered to valid git clones with their reported branches.
func TestList(t *testing.T) {
	root := t.TempDir()
	repo := t.TempDir()
	ws, err := New(Options{ManagedRoot: root, RepoResolver: StaticRepoResolver{"proj": repo}})
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	t.Run("missing project dir is empty", func(t *testing.T) {
		ws.run = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
		got, err := ws.List(context.Background(), "proj")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("got = %#v, want empty", got)
		}
	})

	t.Run("valid clones with branches", func(t *testing.T) {
		projDir := filepath.Join(ws.managedRoot, "proj")
		if err := os.MkdirAll(filepath.Join(projDir, "s1"), 0o755); err != nil {
			t.Fatalf("seed s1: %v", err)
		}
		if err := os.MkdirAll(filepath.Join(projDir, "s2"), 0o755); err != nil {
			t.Fatalf("seed s2: %v", err)
		}
		if err := os.WriteFile(filepath.Join(projDir, "note.txt"), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
		// s3 is a directory whose git command fails — corrupt clone, must be skipped.
		if err := os.MkdirAll(filepath.Join(projDir, "s3-corrupt"), 0o755); err != nil {
			t.Fatalf("seed s3: %v", err)
		}
		ws.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
			joined := strings.Join(args, " ")
			switch {
			case strings.Contains(joined, "-C "+filepath.Join(projDir, "s1")+" branch --show-current"):
				return []byte("feature/one\n"), nil
			case strings.Contains(joined, "-C "+filepath.Join(projDir, "s2")+" branch --show-current"):
				return []byte("feature/two\n"), nil
			case strings.Contains(joined, "-C "+filepath.Join(projDir, "s3-corrupt")+" branch --show-current"):
				return nil, errors.New("fatal: not a git repository")
			}
			t.Fatalf("unexpected git call: %s", joined)
			return nil, nil
		}
		got, err := ws.List(context.Background(), "proj")
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("got %d entries, want 2: %#v", len(got), got)
		}
		bySession := map[domain.SessionID]ports.WorkspaceInfo{}
		for _, e := range got {
			bySession[e.SessionID] = e
		}
		s1, ok := bySession["s1"]
		if !ok || s1.Branch != "feature/one" || s1.Path != filepath.Join(projDir, "s1") {
			t.Fatalf("s1 = %#v", s1)
		}
		s2, ok := bySession["s2"]
		if !ok || s2.Branch != "feature/two" || s2.Path != filepath.Join(projDir, "s2") {
			t.Fatalf("s2 = %#v", s2)
		}
	})
}

func TestOriginsEqual(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"https://github.com/x/y.git", "https://github.com/x/y.git", true},
		{"https://github.com/x/y.git", "https://github.com/other/y.git", false},
		{"git@github.com:x/y.git", "git@github.com:x/y.git", true},
		{"git@github.com:x/y.git", "https://github.com/x/y.git", false},
	}
	for _, tc := range cases {
		if got := originsEqual(tc.a, tc.b); got != tc.want {
			t.Errorf("originsEqual(%q,%q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

// helpers ------------------------------------------------------------------

func mkdirFile(dir, name string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, name), []byte("data"), 0o644)
}
