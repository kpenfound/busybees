{{if .Review}}
## Findings
{{if .Review.Verify}}
No review ran for this round. The list below is the one the review posted on this pull
request{{if .Review.ReviewedHead}} when its head was `{{.Review.ReviewedHead}}`{{end}}, most severe first, and this
round checks whether the developer's new commits addressed it.{{if .Review.ReviewedHead}} `git diff {{.Review.ReviewedHead}}..HEAD`
shows what changed since.{{end}}
{{if .Review.Findings}}
{{.Review.Findings}}
{{else}}
That review's list was empty: the changes this round checks are the ones asked for in
the mail below.
{{end}}
The review is kept under `{{.Review.Artifact}}` (`brief.json`, `angles/`, `findings.json`).
{{else}}
The review ran before this session: the change was sized `{{.Review.Size}}` and reviewed
from the {{join .Review.Angles ", "}} {{if eq (len .Review.Angles) 1}}angle{{else}}angles{{end}}, and the judge merged what they found into the list
below, most severe first. {{if .Review.Summary}}The brief's summary: {{.Review.Summary}}{{end}}
{{- if .Review.Skipped}}

Not reviewed:
{{range .Review.Skipped}}
- {{.}}
{{- end}}
{{- end}}
{{if .Review.Findings}}
{{.Review.Findings}}
{{else}}
The judge's list is empty: no angle found anything to report.
{{end}}
The review is kept under `{{.Review.Artifact}}` (`brief.json`, `angles/`, `findings.json`).
{{end}}
{{- end}}
