# Task: product management pass

## Open milestones (managed by people — read only)
{{if .Milestones}}
| # | Title | Open | Closed | Description |
|---|---|---|---|---|
{{- range .Milestones}}
| {{.Number}} | {{.Title}} | {{.OpenIssues}} | {{.ClosedIssues}} | {{oneline .Description}} |
{{- end}}
{{else}}
_No open milestones._
{{end}}

## Feature issues needing you ({{len .FreshFeatures}})
{{if .FreshFeatures}}
{{- range .FreshFeatures}}
### #{{.Number}}: {{.Title}}
- from: {{.Author.Login}} · {{.CreatedAt.Format "2006-01-02"}} · milestone: {{milestone .}} · proposal: {{if hasLabel .Labels $.Labels.Proposal}}yes{{else}}no{{end}} · {{.URL}}

{{.Body}}
{{- range .Comments}}

**{{.Author.Login}}** ({{.CreatedAt.Format "2006-01-02"}}):

{{.Body}}
{{- end}}
{{end}}
{{else}}
_None: every feature issue is broken down or waiting on a person._
{{end}}

## Proposals awaiting a person's approval ({{len .Proposals}})
{{if .Proposals}}
These are feature issues **you** wrote. Refine them and ask questions on them if you
need to, but do not break any of them into work items until a person approves one by
removing the `{{.Labels.Proposal}}` label.
{{- range .Proposals}}

### #{{.Number}}: {{.Title}}
- from: {{.Author.Login}} · {{.CreatedAt.Format "2006-01-02"}} · milestone: {{milestone .}} · proposal: {{if hasLabel .Labels $.Labels.Proposal}}yes{{else}}no{{end}} · {{.URL}}

{{.Body}}
{{- range .Comments}}

**{{.Author.Login}}** ({{.CreatedAt.Format "2006-01-02"}}):

{{.Body}}
{{- end}}
{{end}}
{{else}}
_None waiting for a person._
{{end}}

## All open feature issues ({{len .Features}})
{{if .Features}}
| # | Milestone | Progress | Proposal | Waiting on person | Title |
|---|---|---|---|---|---|
{{- range .Features}}
| {{.Number}} | {{milestone .}} | {{progress $.Progress .Number}} | {{if hasLabel .Labels $.Labels.Proposal}}yes{{else}}-{{end}} | {{if hasLabel .Labels $.Labels.Proposal}}proposal{{else if hasLabel .Labels $.Labels.Question}}yes{{else}}-{{end}} | {{.Title}} |
{{- end}}
{{else}}
_None._
{{end}}

## Open work items ({{len .Issues}})
{{if .Issues}}
| # | State | Kind | Milestone | Title |
|---|---|---|---|---|
{{- range .Issues}}
| {{.Number}} | {{stateLabel .Labels}} | {{kindLabel .Labels}} | {{milestone .}} | {{.Title}} |
{{- end}}
{{else}}
_No open issues match the factory filter._
{{end}}

## Open pull requests ({{len .PRs}})
{{if .PRs}}
| # | Title | Branch |
|---|---|---|
{{- range .PRs}}
| {{.Number}} | {{.Title}} | {{.HeadRefName}} |
{{- end}}
{{else}}
_None._
{{end}}

## Feedback from people ({{len .Feedback}})
{{if .Feedback}}
{{- range .Feedback}}
### #{{.Number}}: {{.Title}}
- from: {{.Author.Login}} · {{.CreatedAt.Format "2006-01-02"}} · {{.URL}}

{{.Body}}
{{- range .Comments}}

**{{.Author.Login}}** ({{.CreatedAt.Format "2006-01-02"}}):

{{.Body}}
{{- end}}
{{end}}
{{else}}
_None waiting._
{{end}}

## Mail for you ({{len .Inbox}})
{{if .Inbox}}
{{- range .Inbox}}
{{formatMail .}}
{{- end}}
{{else}}
_No new mail._
{{end}}

## Your notes

{{if .Notes}}{{.Notes}}{{else}}_Empty. Start by writing down the product vision as you understand it from the repository._{{end}}
{{template "consolidate" .}}
## Instructions

1. Act on every feedback issue listed above and reply on it (with the marker).
2. For every feature issue needing you: make it detailed enough, ask the person if you
   must (comment + `{{.Labels.Question}}`), otherwise break it into work items and comment
   the list on the feature issue. The proposals listed above are the exception: refine
   them and ask questions on them, but leave them as they are until a person removes the
   `{{.Labels.Proposal}}` label.
3. Reply to every question in your mail.
4. Review the backlog against the vision and the milestones people have set. Create or
   adjust feature issues as needed; close feature issues whose sub-issues are all done.
   Keep the backlog healthy but small.
5. Update your notes file.
6. `done` with `status: done` and a note, or `status: idle`.
