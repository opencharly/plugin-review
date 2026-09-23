package pluginreview

import "strings"

// diffsplit.go — recover per-file patches from a whole-PR unified diff.
//
// Why this exists: GitHub's `GET /repos/{o}/{r}/pulls/{n}/files` omits the
// per-file `patch` field when a file's diff exceeds the API's per-file limit
// (it also omits it for some renamed/binary cases), exposing only
// `filename/status/additions/deletions`. The review assembler's contract is that
// it sees EVERY changed file's full unified diff (context.go), so a file with a
// real (+/-) change but an omitted patch must be recovered — otherwise the model
// reads an empty diff block and the review silently skips the file's substance.
//
// The whole-PR diff (`GET /repos/{o}/{r}/pulls/{n}` with
// `Accept: application/vnd.github.v3.diff`, exposed as ghkit.Client.PRDiff) is
// NOT subject to the per-file omission, so it always carries the hunks. This
// splits it back into one patch per file, keyed by the new-side path.

// splitUnifiedDiff splits a whole-PR unified diff into per-file patches keyed by
// the new-side path (the `b/<path>` from each `diff --git a/<p> b/<p>` header).
// The returned value for a path is the file's complete section (its `diff --git`
// header plus hunks), newline-terminated; it is valid inside a ```diff fence.
func splitUnifiedDiff(diff string) map[string]string {
	out := map[string]string{}
	var curPath string
	var cur strings.Builder
	flush := func() {
		if curPath != "" {
			out[curPath] = strings.TrimRight(cur.String(), "\n") + "\n"
		}
		curPath = ""
		cur.Reset()
	}
	for _, line := range strings.SplitAfter(diff, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			flush()
			curPath = parseGitDiffPath(line)
		}
		if curPath != "" {
			cur.WriteString(line)
		}
	}
	flush()
	return out
}

// parseGitDiffPath extracts the new-side path from a `diff --git a/<p> b/<p>`
// line. Git quotes a path containing spaces/tabs; the quotes are stripped. The
// a/ side is ignored (a rename's new name is what the API's `filename` reports).
func parseGitDiffPath(line string) string {
	s := strings.TrimSuffix(strings.TrimPrefix(line, "diff --git "), "\n")
	i := strings.Index(s, " b/")
	if i < 0 {
		return ""
	}
	p := s[i+3:]
	return strings.Trim(p, "\"")
}
