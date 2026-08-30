package github

import (
	"regexp"
	"strconv"
)

// blockerPhrase matches a "blocked by" / "depends on" phrase followed by one
// or more issue references. The phrase may carry Markdown emphasis and an
// optional colon ("**Depends on**: #3"), and the references may be separated
// by commas, spaces or "and" ("#3, #4 and #5"). Separators are horizontal
// whitespace only, so a reference on a later line is never swallowed.
var blockerPhrase = regexp.MustCompile(`(?i)(?:blocked[ \t]+by|depends[ \t]+on)[*_]*[ \t]*:?[*_]*[ \t]*(#\d+(?:(?:[ \t]*,)?(?:[ \t]*and)?[ \t]*#\d+)*)`)

var issueRef = regexp.MustCompile(`#(\d+)`)

// Blockers returns the issue numbers a body declares as prerequisites, in the
// order they appear and without duplicates. It recognises "blocked by" and
// "depends on" (case-insensitive, anywhere in the body, so the banner form
// "> **Blocked by #37.**" matches) followed by at least one `#N`; the phrase
// on its own ("blocked by the missing tests") declares nothing.
func Blockers(body string) []int {
	var out []int
	seen := map[int]bool{}
	for _, m := range blockerPhrase.FindAllStringSubmatch(body, -1) {
		for _, ref := range issueRef.FindAllStringSubmatch(m[1], -1) {
			n, err := strconv.Atoi(ref[1])
			if err != nil || n <= 0 || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}
