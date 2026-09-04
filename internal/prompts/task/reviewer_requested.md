# Task: review pull request #{{.PR.Number}} (requested by a person)

A person asked for a review of this pull request by putting the
`{{.Labels.ReviewRequested}}` label on it. It is not a pull request a developer
session opened for an issue: there is no issue and no developer to send changes back
to. The label has already been removed. This is one review pass; the person adds the
label again to ask for another.

## Pull request #{{.PR.Number}}: {{.PR.Title}}
{{.PR.URL}} — branch `{{.PR.HeadRefName}}` → `{{.PR.BaseRefName}}` · author: {{.PR.Author.Login}}

{{.PR.Body}}

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

Read the diff (`gh pr diff {{.PR.Number}} -R {{.Project.Repo}}`) and the pull request
itself with `pr_view`, then report `done` with `status: approved` or
`status: changes-requested` and a note saying what you found.

Update your notes file before you finish.
