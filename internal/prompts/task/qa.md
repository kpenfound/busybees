# Task: QA pass on `{{.Project.DefaultBranch}}`

{{if .LastRun}}Your last run was {{.LastRun}}.{{else}}This is your first run.{{end}}

## Merged since your last run ({{len .MergedPRs}})
{{if .MergedPRs}}
{{- range .MergedPRs}}
### PR #{{.Number}}: {{.Title}}
{{.URL}}{{if .ClosingIssues}} · closes {{range .ClosingIssues}}#{{.}} {{end}}{{end}}

{{.Body}}
{{end}}
{{else}}
_Nothing new was merged; do a general exploratory pass._
{{end}}

## Open bugs already filed
{{if .Issues}}
| # | State | Title |
|---|---|---|
{{- range .Issues}}
| {{.Number}} | {{stateLabel .Labels}} | {{.Title}} |
{{- end}}
{{else}}
_None._
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

Set up, run the tests, exercise the product, verify the merged changes, file a bug for
every defect you reproduced (none, if the batch is clean), send the product manager your
report, update your notes, then report `done` with `status: done` and a note.
