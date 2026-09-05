{{template "interrupted" .}}# Task: review pull request #{{.PR.Number}} (round {{.Round}} of {{.MaxRounds}})

## Pull request #{{.PR.Number}}: {{.PR.Title}}
{{.PR.URL}} — branch `{{.PR.HeadRefName}}` → `{{.PR.BaseRefName}}` · author: {{.PR.Author.Login}}

{{.PR.Body}}

## Issue #{{.Issue.Number}}: {{.Issue.Title}}
{{.Issue.URL}}

{{.Issue.Body}}
{{if .ChecksStatus}}
## Required checks
{{if .Checks}}{{range .Checks}}
- {{.Name}} — {{.Bucket}}{{if .Link}} — {{.Link}}{{end}}
{{- end}}

{{if eq .ChecksStatus "passed"}}CI is green; concentrate on the change itself.{{else}}Checks were still pending after `{{.ChecksTimeout}}`: judge the change on the code, and say in your note that CI had not reported when you reviewed.{{end}}
{{else}}
This repository reports no required checks: nothing was verified for you. Judge the
change on the code, and say so in your note.
{{end}}{{end}}
{{if .PreviousRounds}}
## Your feedback from previous rounds
{{- range .PreviousRounds}}
{{formatMail .}}
{{- end}}
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
{{template "stages" .}}
## Instructions

The PR branch is checked out in your working directory. Read the diff and the issue,
{{if .Stages}}work through every stage above, {{end}}then either report `done` with
`status: approved` and a note, or send your feedback to the developer with `mail_send`
(`to: developer`, `pr: {{.PR.Number}}`, `issue: {{.Issue.Number}}`) and report `done`
with `status: changes-requested`.{{if .Stages}} One message, its points **grouped by
stage** in the stages' order, each group headed by that stage's verdict line.{{end}}
{{if gt .Round 1}}
This is a follow-up review, not a fresh one: go through `## Your feedback from
previous rounds` point by point and say whether each was addressed, then judge the
change as it now stands against every stage above, the same as a first review. The
scope stays full — do not narrow it to the commits made since last round, and do not
widen it into extra scrutiny of them either. Read the whole diff, and raise anything
you missed in round 1 too.
{{end}}
{{if ge .Round .MaxRounds}}
This is the final review round. If the PR is still not mergeable, request changes anyway;
the orchestrator will escalate it to a human.
{{end}}
Update your notes file before you finish.
