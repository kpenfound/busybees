{{if .Review}}
## Findings

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
