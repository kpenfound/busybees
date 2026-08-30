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

{{if eq .ChecksStatus "passed"}}CI is green; concentrate on the change itself.{{else}}Checks were still pending after `{{.ChecksTimeout}}`; run the repository's test-suite yourself.{{end}}
{{else}}
This repository reports no required checks; run the tests yourself.
{{end}}{{end}}
{{if .PreviousRounds}}
## Your feedback from previous rounds
{{- range .PreviousRounds}}
{{formatMail .}}
{{- end}}
{{end}}
## Your notes

{{if .Notes}}{{.Notes}}{{else}}_Empty._{{end}}

## Instructions

The PR branch is checked out in your working directory. Review the diff, run the tests,
then either `bees done approved -m "..."` or send your feedback to the developer with
`bees mail send --to developer --pr {{.PR.Number}} --issue {{.Issue.Number}} ...` and
`bees done changes-requested`.
{{if ge .Round .MaxRounds}}
This is the final review round. If the PR is still not mergeable, request changes anyway;
the orchestrator will escalate it to a human.
{{end}}
Update your notes file before you finish.
