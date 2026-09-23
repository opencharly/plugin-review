package pluginreview

import (
	"strconv"
	"strings"
)

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
// line. It handles BOTH header forms git emits:
//
//	diff --git a/plain/path.yml b/plain/path.yml
//	diff --git "a/odd path\twith \"quotes\"" "b/odd path\twith \"quotes\""
//
// The second (C-quoted) form is used under `core.quotePath` for paths with
// spaces, tabs, `"`/`\`, or non-ASCII bytes. The returned value is the UNQUOTED
// new name, so it matches the API's `filename` key used for recovery.
func parseGitDiffPath(line string) string {
	s := strings.TrimSuffix(strings.TrimPrefix(line, "diff --git "), "\n")
	// Quoted form: the second token begins ` "b/` (a space then an opening quote).
	if i := strings.Index(s, ` "b/`); i >= 0 {
		return strings.TrimPrefix(unquoteGitPath(s[i+1:]), "b/")
	}
	// Unquoted form: the second token begins ` b/`.
	if i := strings.Index(s, " b/"); i >= 0 {
		return s[i+3:]
	}
	return ""
}

// unquoteGitPath decodes a git C-quoted path token (including its surrounding
// double quotes) to the literal path: it reverses the escapes git emits under
// core.quotePath — \\, \", \t, \n, \r, \a, \b, \f, \v and octal \NNN.
func unquoteGitPath(tok string) string {
	tok = strings.TrimSpace(tok)
	if len(tok) < 2 || tok[0] != '"' {
		return tok
	}
	end := strings.LastIndexByte(tok, '"')
	if end <= 0 {
		return strings.Trim(tok, "\"")
	}
	inner := tok[1:end]
	var b strings.Builder
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if c != '\\' || i+1 >= len(inner) {
			b.WriteByte(c)
			continue
		}
		i++
		switch inner[i] {
		case '\\', '"':
			b.WriteByte(inner[i])
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'v':
			b.WriteByte('\v')
		default:
			if inner[i] >= '0' && inner[i] <= '7' {
				j := i
				for j < len(inner) && j < i+3 && inner[j] >= '0' && inner[j] <= '7' {
					j++
				}
				if v, err := strconv.ParseUint(inner[i:j], 8, 8); err == nil {
					b.WriteByte(byte(v))
				}
				i = j - 1
			} else {
				b.WriteByte(inner[i])
			}
		}
	}
	return b.String()
}
