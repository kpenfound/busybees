{{with .Release}}{{with .Milestone}}# Task: ship milestone {{.Title}}

Milestone #{{.Number}} `{{.Title}}` has {{.ClosedIssues}} closed and {{.OpenIssues}} open issues, and no open
pull request is in flight for it. Shipping it tags `{{$.Project.DefaultBranch}}` as `{{.Title}}`.
{{- if .Description}}

{{.Description}}
{{- end}}
{{end}}
{{if .Issue}}Issue #{{.Issue}} is a closed issue in the milestone: pass `related: {{.Issue}}` to every
`issue_create` call, so the issue you file inherits the milestone.{{else}}The scheduler found no closed issue in the milestone to relate to: pass
`milestone: "{{.Milestone.Title}}"` to every `issue_create` call instead, so the issue you file is in the milestone.{{end}}
{{end}}
## Mail for you ({{len .Inbox}})
{{if .Inbox}}
{{- range .Inbox}}
{{formatMail .}}
{{- end}}
{{else}}
_No new mail._
{{end}}

{{template "consolidate" .}}
## Instructions

Check the release workflow on `{{.Project.DefaultBranch}}`. If no workflow meets the release
contract, file one developer work item for it and ship nothing. Otherwise call `release_ship`
(`milestone: {{with .Release}}{{.Milestone.Number}}{{end}}`). If it refuses an invalid or existing tag, file an issue
labelled `{{.Labels.NeedsHuman}}` naming the milestone and the reason. Update your notes
(`notes_write`), then report `done` with a note, or `failed`.
