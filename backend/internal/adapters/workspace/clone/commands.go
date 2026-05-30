package clone

// remoteGetURLArgs returns the args to read the configured origin URL of a
// repository at dir. Used by Create to discover what URL to clone from and by
// Restore to verify an existing clone points at the expected source.
func remoteGetURLArgs(dir string) []string {
	return []string{"-C", dir, "remote", "get-url", "origin"}
}

// cloneArgs returns the args to perform a full clone of source into dest.
// Plain `git clone` (no --reference, no --shared) is used deliberately: each
// destination gets an independent object database, so concurrent clones from
// the same source touch only the source's read-only pack files and never
// contend on .git/index.lock or alternates. See package doc for the full
// concurrency model.
func cloneArgs(source, dest string) []string {
	return []string{"clone", source, dest}
}

// checkoutNewBranchArgs creates a new local branch starting from HEAD.
func checkoutNewBranchArgs(dir, branch string) []string {
	return []string{"-C", dir, "checkout", "-b", branch}
}

// checkoutBranchArgs checks out an existing branch (used as fallback when
// checkoutNewBranchArgs reports the branch already exists).
func checkoutBranchArgs(dir, branch string) []string {
	return []string{"-C", dir, "checkout", branch}
}

// showCurrentBranchArgs returns the short name of the currently checked-out
// branch (empty for detached HEAD).
func showCurrentBranchArgs(dir string) []string {
	return []string{"-C", dir, "branch", "--show-current"}
}

// statusPorcelainArgs returns the args used by Destroy to detect uncommitted
// changes. Any non-empty output means the working tree has work the agent has
// not committed; Destroy refuses to delete it.
//
// This is the clone-adapter analogue of gitworktree's intentional omission of
// `--force` from `git worktree remove`: we never blindly delete a session
// directory that may contain unsaved agent work (review item RA on PR #23).
// There is no Force escape hatch on the port or on this adapter — the
// safety check is unconditional.
func statusPorcelainArgs(dir string) []string {
	return []string{"-C", dir, "status", "--porcelain"}
}

// revParseInsideWorkTreeArgs returns the args used to confirm a directory is
// a valid git working tree (used by List to skip corrupt clones and by
// Restore to validate a recovered directory before reusing it).
func revParseInsideWorkTreeArgs(dir string) []string {
	return []string{"-C", dir, "rev-parse", "--is-inside-work-tree"}
}
