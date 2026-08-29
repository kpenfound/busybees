# Task: implement issue #{{.Issue.Number}}{{if gt .Round 1}} (review round {{.Round}} of {{.MaxRounds}}){{end}}

## Issue #{{.Issue.Number}}: {{.Issue.Title}}
- author: {{.Issue.Author.Login}} · labels: {{labels .Issue.Labels}} · milestone: {{milestone .Issue}} · {{.Issue.URL}}
{{- if .Parent}}
- part of feature #{{.Parent.Number}}: {{.Parent.Title}} (read it for context)
{{- end}}

{{.Issue.Body}}
{{if .Issue.Comments}}
### Comments
{{- range .Issue.Comments}}

**{{.Author.Login}}** ({{.CreatedAt.Format "2006-01-02"}}):

{{.Body}}
{{- end}}
{{end}}
{{if .PR}}
## Existing pull request #{{.PR.Number}}: {{.PR.Title}}
{{.PR.URL}} — branch `{{.PR.HeadRefName}}` → `{{.PR.BaseRefName}}`

{{.PR.Body}}
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

{{if .Notes}}{{.Notes}}{{else}}_Empty._{{end}}

## Instructions

You are on branch `{{.Branch}}`, based on `{{.Project.DefaultBranch}}`.
{{if .PR -}}
A pull request already exists. Address the review feedback in your mail, push, update the
PR description if needed, then `bees done pr-updated --pr {{.PR.Number}}`.
{{- else -}}
Implement the issue, push, open the pull request (body must include
`Closes #{{.Issue.Number}}`), then `bees done pr-opened --pr <number>`.
{{- end}}
If you must ask the project manager something first, send the mail and `bees done question`.
Update your notes file before you finish.
