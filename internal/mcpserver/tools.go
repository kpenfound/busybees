package mcpserver

import (
	"context"
	"errors"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/issues"
	"github.com/kpenfound/busybees/internal/mail"
)

// ---- mail ------------------------------------------------------------------

type mailSendInput struct {
	To        string `json:"to" jsonschema:"the role to write to"`
	Subject   string `json:"subject" jsonschema:"subject line, one short sentence"`
	Body      string `json:"body" jsonschema:"the message, markdown; say what you need and why"`
	Issue     int    `json:"issue,omitempty" jsonschema:"issue the message is about (defaults to the issue this session is working on)"`
	PR        int    `json:"pr,omitempty" jsonschema:"pull request the message is about (defaults to the PR this session is working on)"`
	InReplyTo string `json:"in_reply_to,omitempty" jsonschema:"id of the message being answered"`
}

type mailListInput struct {
	Unread bool `json:"unread,omitempty" jsonschema:"only messages that have not been delivered yet"`
	Issue  int  `json:"issue,omitempty" jsonschema:"only messages about this issue"`
	PR     int  `json:"pr,omitempty" jsonschema:"only messages about this pull request"`
}

func (s *server) addMailTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "mail_send",
		Title: "Send mail to another role",
		Description: "Send a message to another role in the factory. The mailbox is the only " +
			"channel between roles: never talk to a role through a GitHub comment. Attach the " +
			"issue and/or PR the message is about so it reaches the session working on it; both " +
			"default to this session's issue and PR.",
		InputSchema: schemaFor[mailSendInput](map[string][]string{"to": config.Roles}),
	}, s.mailSend)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "mail_list",
		Title:       "Read the mailbox",
		Description: "List messages in the local mailbox in full. Mail addressed to this session is already included in the task prompt; use this to look further back.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schemaFor[mailListInput](nil),
	}, s.mailList)
}

func (s *server) mailSend(ctx context.Context, _ *mcp.CallToolRequest, in mailSendInput) (*mcp.CallToolResult, any, error) {
	if s.mail == nil {
		return nil, nil, errors.New("no mailbox: $BEES_STATE_DIR is not set")
	}
	to, err := config.CanonicalRole(in.To)
	if err != nil {
		return nil, nil, err
	}
	if s.env.Role == "" {
		return nil, nil, errors.New("mail can only be sent from a session ($BEES_ROLE is not set)")
	}
	if in.Issue == 0 {
		in.Issue = s.env.Issue
	}
	if in.PR == 0 {
		in.PR = s.env.PR
	}
	m, err := s.mail.Send(mail.Message{
		To: to, From: s.env.Role, Subject: in.Subject, Body: in.Body,
		Issue: in.Issue, PR: in.PR, InReplyTo: in.InReplyTo,
	})
	if err != nil {
		return nil, nil, err
	}
	return text("sent %s to %s", m.ID, m.To), nil, nil
}

func (s *server) mailList(ctx context.Context, _ *mcp.CallToolRequest, in mailListInput) (*mcp.CallToolResult, any, error) {
	if s.mail == nil {
		return nil, nil, errors.New("no mailbox: $BEES_STATE_DIR is not set")
	}
	msgs, err := s.mail.List(mail.Filter{Issue: in.Issue, PR: in.PR, UnreadOnly: in.Unread})
	if err != nil {
		return nil, nil, err
	}
	if len(msgs) == 0 {
		return text("no messages"), nil, nil
	}
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(mail.Format(m))
	}
	return text("%s", b.String()), nil, nil
}

// ---- issues ----------------------------------------------------------------

type issueCreateInput struct {
	Title     string   `json:"title" jsonschema:"issue title"`
	Body      string   `json:"body" jsonschema:"issue body, markdown"`
	Parent    int      `json:"parent,omitempty" jsonschema:"feature issue to attach this one to as a GitHub sub-issue; its milestone is inherited"`
	Related   int      `json:"related,omitempty" jsonschema:"issue whose milestone to inherit without attaching (a bug found while working on it, a feature distilled from feedback)"`
	Milestone string   `json:"milestone,omitempty" jsonschema:"milestone title, overriding what would be inherited; people own milestones, so leave this empty unless you were told otherwise"`
	Bug       bool     `json:"bug,omitempty" jsonschema:"the work item is a bug"`
	Feature   bool     `json:"feature,omitempty" jsonschema:"a feature issue owned by the product manager, not a work item"`
	Ready     bool     `json:"ready,omitempty" jsonschema:"the work item is already detailed enough to build: skip triage"`
	Labels    []string `json:"labels,omitempty" jsonschema:"extra labels on top of the factory's own"`
	BlockedBy []int    `json:"blocked_by,omitempty" jsonschema:"issues this one must not be built before; written as a 'Blocked by #N' line the scheduler honours (no GitHub relationship is created)"`
}

type issueLinkInput struct {
	Parent int `json:"parent" jsonschema:"feature issue number"`
	Child  int `json:"child" jsonschema:"issue to attach as a sub-issue"`
}

func (s *server) addIssueTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "issue_create",
		Title: "Create a factory issue",
		Description: "Create a GitHub issue the way the factory needs it: with the visibility " +
			"label and assignee, the kind and state labels, attached to its parent feature as a " +
			"sub-issue and in the inherited milestone. Always use this instead of `gh issue create`.",
		InputSchema: schemaFor[issueCreateInput](nil),
	}, s.issueCreate)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "issue_link",
		Title: "Attach an issue to a feature",
		Description: "Attach an existing issue to a feature issue as a GitHub sub-issue, so the feature's " +
			"progress shows on GitHub. It also puts the issue in the feature's milestone when the issue " +
			"is in none; a milestone it already has is left alone.",
		InputSchema: schemaFor[issueLinkInput](nil),
	}, s.issueLink)
}

func (s *server) issueCreate(ctx context.Context, _ *mcp.CallToolRequest, in issueCreateInput) (*mcp.CallToolResult, any, error) {
	if s.issues == nil {
		return nil, nil, errors.New("issues are unavailable: bees.toml could not be loaded")
	}
	kind := issues.KindTask
	switch {
	case in.Bug && in.Feature:
		return nil, nil, errors.New("bug and feature are exclusive")
	case in.Bug:
		kind = issues.KindBug
	case in.Feature:
		kind = issues.KindFeature
	}
	res, err := s.issues.Create(ctx, issues.Options{
		Title: in.Title, Body: in.Body, Kind: kind, Parent: in.Parent, Related: in.Related,
		Milestone: in.Milestone, ExtraLabels: in.Labels, Ready: in.Ready, BlockedBy: in.BlockedBy,
	})
	if err != nil {
		return nil, nil, err
	}
	return text("%s", res), nil, nil
}

func (s *server) issueLink(ctx context.Context, _ *mcp.CallToolRequest, in issueLinkInput) (*mcp.CallToolResult, any, error) {
	if s.issues == nil {
		return nil, nil, errors.New("issues are unavailable: bees.toml could not be loaded")
	}
	res, err := s.issues.Link(ctx, in.Parent, in.Child)
	if err != nil {
		return nil, nil, err
	}
	return text("%s", res), nil, nil
}
