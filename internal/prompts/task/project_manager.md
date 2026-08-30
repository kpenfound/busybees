# Task: project management pass

## Issues to triage ({{len .TriageIssues}})
{{if .TriageIssues}}
{{- range .TriageIssues}}
### #{{.Number}}: {{.Title}}
- author: {{.Author.Login}} · labels: {{labels .Labels}} · milestone: {{milestone .}} · parent feature: {{parentOf $.Parents .Number}}{{$b := blockedBy $.Blockers .Number}}{{if ne $b "-"}} · blocked by: {{$b}} (open){{end}} · {{.URL}}

{{.Body}}
{{end}}
{{else}}
_Nothing to triage._
{{end}}

## Mail for you ({{len .Inbox}})
{{if .Inbox}}
{{- range .Inbox}}
{{formatMail .}}
{{- end}}
{{else}}
_No new mail._
{{end}}

## Other open factory issues
{{if .Issues}}
| # | State | Kind | Blocked by | Milestone | Title |
|---|---|---|---|---|---|
{{- range .Issues}}
| {{.Number}} | {{stateLabel .Labels}} | {{kindLabel .Labels}} | {{blockedBy $.Blockers .Number}} | {{milestone .}} | {{.Title}} |
{{- end}}
{{else}}
_None._
{{end}}

## Your notes

{{if .Notes}}{{.Notes}}{{else}}_Empty._{{end}}

## Instructions

1. Answer every question in your mail first (developers are blocked on you).
2. Triage each issue listed above: refine and move to `{{.Labels.Ready}}`, split, ask the
   product manager (and move to `{{.Labels.Blocked}}`), or close.
3. Update your notes file.
4. `bees done done -m "..."` or `bees done idle`.
