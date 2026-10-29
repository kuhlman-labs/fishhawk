package gitops

// urlPushNoTrackingRemote is the reserved name of the command-scope remote
// that urlPushNoTrackingArgs adds to a URL push. See that function.
const urlPushNoTrackingRemote = "fishhawk-url-push-untracked"

// urlPushNoTrackingArgs returns the `git -c` prefix that keeps a URL push
// (`git push <remoteURL> ...`) from creating or moving a remote-tracking ref
// (#4767).
//
//  1. The behaviour. git 2.56.0 (RelNotes: "When the push remote is specified
//     as a URL, the fetch refspec of a uniquely matching configured remote is
//     now used to find and update the remote-tracking branch"; topic tip
//     93775e35b7ed) makes transport_push call repo_remote_for_push_tracking,
//     skipped only for TRANSPORT_PUSH_DRY_RUN|TRANSPORT_RECURSE_SUBMODULES_ONLY.
//     A URL push therefore writes refs/remotes/<r>/<b> whenever exactly one
//     configured remote has that push URL. Earlier git never did, and the
//     #770 contract (UpdateTrackingRef) and the force-with-lease logic in
//     CommitAndPush rely on that.
//  2. No config key. git 2.56 has no push.* or remote.* key that switches the
//     update off, so there is nothing to pin.
//  3. The mechanism. The prefix adds a SECOND configured remote at command
//     scope with the same push URL, so the match is no longer unique and git
//     keeps the anonymous URL remote, which has no fetch refspec. When the
//     extra remote is the ONLY match (an SSH origin and an HTTPS push), it is
//     chosen but also has no fetch refspec. Either way remote_find_tracking
//     finds nothing and no ref is written. pushurl, not url, so the extra
//     remote takes part only in push matching.
//  4. On git <= 2.55 the pair is a no-op.
//  5. Residuals. A repository that already defines a remote named
//     urlPushNoTrackingRemote gets the URL appended to its pushurl list for
//     the command; and a future git that changes the uniqueness rule defeats
//     the mechanism. TestURLPush_LeavesNoTrackingRef_OnInstalledGit reddens on
//     such a git.
func urlPushNoTrackingArgs(remoteURL string) []string {
	return []string{"-c", "remote." + urlPushNoTrackingRemote + ".pushurl=" + remoteURL}
}
