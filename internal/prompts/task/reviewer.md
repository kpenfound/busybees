# Task: review pull request #{{.PR.Number}} (round {{.Round}} of {{.MaxRounds}})

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
## Your notes

{{if .Notes}}{{.Notes}}{{else}}_Empty._{{end}}
{{template "consolidate" .}}
## Instructions

The PR branch is checked out in your working directory. Read the diff and the issue,
then either report `done` with `status: approved` and a note, or send your feedback to
the developer with `mail_send` (`to: developer`, `pr: {{.PR.Number}}`,
`issue: {{.Issue.Number}}`) and report `done` with `status: changes-requested`.
{{if ge .Round .MaxRounds}}
This is the final review round. If the PR is still not mergeable, request changes anyway;
the orchestrator will escalate it to a human.
{{end}}
Update your notes file before you finish.
