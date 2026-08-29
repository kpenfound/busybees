# Task: review pull request #{{.PR.Number}} (round {{.Round}} of {{.MaxRounds}})

## Pull request #{{.PR.Number}}: {{.PR.Title}}
{{.PR.URL}} — branch `{{.PR.HeadRefName}}` → `{{.PR.BaseRefName}}` · author: {{.PR.Author.Login}}

{{.PR.Body}}

## Issue #{{.Issue.Number}}: {{.Issue.Title}}
{{.Issue.URL}}

{{.Issue.Body}}
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
