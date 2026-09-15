package review

import (
	"regexp"
	"strings"
)

// backticks matches a run of three or more backticks, the fences a piece of
// context can already contain.
var backticks = regexp.MustCompile("`{3,}")

// fenced puts content in a code fence longer than any fence inside it, so a
// diff of a markdown file cannot end the block early.
func fenced(content string) string {
	fence := "```"
	for _, run := range backticks.FindAllString(content, -1) {
		if len(run) >= len(fence) {
			fence = strings.Repeat("`", len(run)+1)
		}
	}
	return fence + "\n" + strings.TrimRight(content, "\n") + "\n" + fence
}
