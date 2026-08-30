# Task: project management pass
{{- $overflow := false}}{{range .Issues}}{{if hasLabel .Labels $.Labels.Triage}}{{$overflow = true}}{{end}}{{end}}

## Issues to triage ({{len .TriageIssues}} shown{{if $overflow}}, more below{{end}})
{{if .TriageIssues}}
{{- range .TriageIssues}}
### #{{.Number}}: {{.Title}}
- author: {{.Author.Login}} · labels: {{labels .Labels}} · milestone: {{milestone .}} · parent feature: {{parentOf $.Parents .Number}}{{$b := blockedBy $.Blockers .Number}}{{if ne $b "-"}} · blocked by: {{$b}} (open){{end}} · {{.URL}}

{{.Body}}
{{end}}
{{else}}
_Nothing to triage._
{{end}}
{{- if $overflow}}
## Also in `{{.Labels.Triage}}`, bodies not shown

Your triage list is capped at `scheduler.triage_batch_size` issues per pass and these did
not fit. They are triage work too: read one with `issue_view` and refine it if you have
room. Anything you leave comes back next pass.

| # | Kind | Blocked by | Milestone | Title |
|---|---|---|---|---|
{{- range .Issues}}{{if hasLabel .Labels $.Labels.Triage}}
| {{.Number}} | {{kindLabel .Labels}} | {{blockedBy $.Blockers .Number}} | {{milestone .}} | {{.Title}} |
{{- end}}{{end}}
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
{{- $others := false}}{{range .Issues}}{{if not (hasLabel .Labels $.Labels.Triage)}}{{$others = true}}{{end}}{{end}}
{{if $others}}
| # | State | Kind | Blocked by | Milestone | Title |
|---|---|---|---|---|---|
{{- range .Issues}}{{if not (hasLabel .Labels $.Labels.Triage)}}
| {{.Number}} | {{stateLabel .Labels}} | {{kindLabel .Labels}} | {{blockedBy $.Blockers .Number}} | {{milestone .}} | {{.Title}} |
{{- end}}{{end}}
{{else}}
_None._
{{end}}

## Your notes

{{if .Notes}}{{.Notes}}{{else}}_Empty._{{end}}
{{template "consolidate" .}}
## Instructions

1. Answer every question in your mail first (developers are blocked on you).
2. Triage each issue listed above: refine and move to `{{.Labels.Ready}}`, split, ask the
   product manager (and move to `{{.Labels.Blocked}}`), or close.{{if $overflow}} The queue is
   larger than this pass's batch: take from the `{{.Labels.Triage}}` table too if you have
   room.{{end}}
3. Update your notes file.
4. `done` with `status: done` and a note, or `status: idle`.
