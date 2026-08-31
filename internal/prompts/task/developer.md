{{template "interrupted" .}}{{if gt .Retry 0}}**Your previous attempt was interrupted before it finished.** The branch may
already contain partial work — inspect the working tree and the branch's
commits before writing anything, and continue from there rather than starting
over.

{{end}}# Task: implement issue #{{.Issue.Number}}{{if gt .Round 1}} (review round {{.Round}} of {{.MaxRounds}}){{end}}

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
{{template "consolidate" .}}
## Instructions

You are on branch `{{.Branch}}`, based on `{{.Project.DefaultBranch}}`.
{{if .PR -}}
A pull request already exists. Address the review feedback in your mail, merge the
default branch, push, update the PR description if needed, then `done`
(`status: pr-updated`, `pr: {{.PR.Number}}`).
{{- else -}}
Implement the issue, push, open the pull request (body must include
`Closes #{{.Issue.Number}}`), then `done` (`status: pr-opened`, `pr: <number>`).
{{- end}}
If you must ask the project manager something first, send the mail and report `done`
with `status: question`.
Update your notes file before you finish.
