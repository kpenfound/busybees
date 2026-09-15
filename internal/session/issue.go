package session

import (
	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/internal/ghwork"
)

const WorkFile = agent.WorkFile

var WriteWork = agent.WriteWork
var ReadWork = agent.ReadWork

// WriteIssue adapts the GitHub issue used by busybees to a session work marker.
func WriteIssue(dir string, number int) error { return WriteWork(dir, ghwork.New(number, 0)) }
func ReadIssue(dir string) int                { ref, _ := ReadWork(dir); return ghwork.Issue(ref) }
