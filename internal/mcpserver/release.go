package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/kpenfound/busybees/internal/github"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type releaseShipInput struct {
	Milestone int `json:"milestone" jsonschema:"number of the open milestone to ship; this exact milestone will be closed"`
}

// releaseFailure keeps completed remote writes in the response even when a
// later operation fails. MCP errors alone would lose that context.
func releaseFailure(completed []string, format string, args ...any) (*mcp.CallToolResult, any, error) {
	message := "Stopped: " + fmt.Sprintf(format, args...)
	if len(completed) > 0 {
		message = strings.Join(completed, "\n") + "\n" + message
	}
	r := text("%s", message)
	r.IsError = true
	return r, nil, nil
}

func (s *server) releaseShip(ctx context.Context, _ *mcp.CallToolRequest, in releaseShipInput) (*mcp.CallToolResult, any, error) {
	if s.github == nil {
		return nil, nil, errNoGitHub
	}
	var completed []string
	if in.Milestone <= 0 {
		return releaseFailure(completed, "provide a positive milestone number")
	}
	milestones, err := s.github.ListMilestones(ctx)
	if err != nil {
		return releaseFailure(completed, "could not read open milestones: %v", err)
	}
	var title string
	for _, m := range milestones {
		if m.Number == in.Milestone {
			title = m.Title
			if m.ClosedIssues == 0 || m.OpenIssues != 0 {
				return releaseFailure(completed, "milestone #%d (%s) needs at least one closed issue and no open issues (closed: %d, open: %d)", m.Number, m.Title, m.ClosedIssues, m.OpenIssues)
			}
			break
		}
	}
	if title == "" {
		return releaseFailure(completed, "milestone #%d is not open; nothing was shipped or closed", in.Milestone)
	}
	if !validReleaseTag(title) {
		return releaseFailure(completed, "milestone #%d title %q is not a valid Git tag", in.Milestone, title)
	}
	prs, err := s.github.ListOpenPRs(ctx)
	if err != nil {
		return releaseFailure(completed, "could not inspect open pull requests: %v", err)
	}
	pr, issueNumber, err := github.MilestoneInFlight(ctx, prs, title, s.github.Issue)
	switch {
	case err != nil:
		return releaseFailure(completed, "could not check issue #%d referenced by open pull request #%d: %v", issueNumber, pr, err)
	case pr != 0 && issueNumber == 0:
		return releaseFailure(completed, "pull request #%d is open in milestone #%d", pr, in.Milestone)
	case pr != 0:
		return releaseFailure(completed, "pull request #%d is still open for issue #%d in milestone #%d", pr, issueNumber, in.Milestone)
	}
	exists, err := s.github.TagExists(ctx, title)
	if err != nil {
		return releaseFailure(completed, "could not check whether tag %q exists: %v", title, err)
	}
	if exists {
		return releaseFailure(completed, "tag %q already exists; inspect the prior release attempt before retrying", title)
	}
	sha, err := s.github.BranchHead(ctx, "main")
	if err != nil {
		return releaseFailure(completed, "could not read main head: %v", err)
	}
	if err := s.github.CreateTag(ctx, title, sha); err != nil {
		return releaseFailure(completed, "could not create and push tag %q at main commit %s: %v; check whether the tag was created before retrying", title, sha, err)
	}
	completed = append(completed, fmt.Sprintf("Tag %s pushed at main commit %s.", title, sha))
	if err := s.github.CreateRelease(ctx, title); err != nil {
		return releaseFailure(completed, "could not create GitHub release for %s: %v; milestone #%d remains open", title, err, in.Milestone)
	}
	completed = append(completed, fmt.Sprintf("GitHub release %s created with generated notes.", title))
	if err := s.github.CloseMilestone(ctx, in.Milestone); err != nil {
		return releaseFailure(completed, "could not close milestone #%d: %v", in.Milestone, err)
	}
	completed = append(completed, fmt.Sprintf("Milestone #%d closed.", in.Milestone))
	return text("%s", strings.Join(completed, "\n")), nil, nil
}

// validReleaseTag follows git-check-ref-format's restrictions for the full
// refs/tags/<title> ref. Titles stay one ref component: no nested tags.
func validReleaseTag(tag string) bool {
	if tag == "" || tag == "@" || strings.HasPrefix(tag, ".") || strings.HasSuffix(tag, ".") || strings.HasSuffix(tag, ".lock") || strings.Contains(tag, "..") || strings.Contains(tag, "@{") || strings.ContainsAny(tag, " /\\~^:?*[") {
		return false
	}
	for _, c := range tag {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}
