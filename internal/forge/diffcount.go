// SPDX-License-Identifier: MIT

package forge

import "strings"

// CountPatch counts added and deleted lines in unified-diff text. File
// headers ("+++ ", "--- ") are ignored, so it works both for full patches and
// for hunk-only text as returned by the GitLab and GitHub file APIs.
func CountPatch(patch string) (additions, deletions int) {
	inHunk := false
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case strings.HasPrefix(line, "diff --git "):
			inHunk = false
		case !inHunk:
			continue
		case strings.HasPrefix(line, "+"):
			additions++
		case strings.HasPrefix(line, "-"):
			deletions++
		}
	}
	return additions, deletions
}
