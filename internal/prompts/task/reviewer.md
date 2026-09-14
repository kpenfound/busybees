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

{{if eq .ChecksStatus "passed"}}CI is green.{{else}}Checks were still pending after `{{.ChecksTimeout}}`: say in your note that CI had not reported when you reviewed.{{end}}
{{else}}
This repository reports no required checks: nothing was verified for you. Say so in
your note.
{{end}}{{end}}
## Mail for you ({{len .Inbox}})
{{if .Inbox}}
{{- range .Inbox}}
{{formatMail .}}
{{- end}}
{{else}}
_No new mail._
{{end}}

{{template "consolidate" .}}
{{template "findings" .}}
## Instructions

The PR branch is checked out in your working directory. Post the findings above on the
pull request as one `comment` review with `submit_review` (`number: {{.PR.Number}}`), the
verdict line first and every finding after it, then either report `done` with
`status: approved` and a note, or send the findings to the developer with `mail_send`
(`to: developer`, `pr: {{.PR.Number}}`, `issue: {{.Issue.Number}}`) and report `done`
with `status: changes-requested`. Post the list as it is: nothing dropped, nothing added.
{{if gt .Round 1}}
This is round {{.Round}}: the review ran again on the change as it now stands, so the
findings above are about the current head, not the developer's last round. Post and
judge them the same as a first review.
{{end -}}
{{if ge .Round .MaxRounds}}
This is the final review round. If a finding still needs fixing, request changes anyway;
the orchestrator will escalate it to a human.
{{end}}
Update your notes (`notes_write`) before you finish.
