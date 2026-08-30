// Package text renders small pieces of English the bees print.
package text

import "fmt"

// Count renders a count with its noun, singular for one: "1 session",
// "0 sessions", "2 sessions". Only the regular plural (append "s") is
// handled — every noun bees count is regular, and an inflection library
// would be a lot of machinery for "session", "turn" and "warning".
func Count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
