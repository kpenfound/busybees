{{if .Review}}
## Findings
{{if .Review.Verify}}
No review ran for this round. The list below is the one the review posted on this pull
request{{if .Review.ReviewedHead}} when its head was `{{.Review.ReviewedHead}}`{{end}}, {{if .Review.JevRanked}}by Jev probability, highest first, with eliminated findings removed{{else}}most severe first{{end}}, and this
round checks whether the developer's new commits addressed it.{{if .Review.ReviewedHead}} `git diff {{.Review.ReviewedHead}}..HEAD`
shows what changed since.{{end}}
{{if .Review.Findings}}
{{.Review.Findings}}
{{else}}
That review's list was empty: the changes this round checks are the ones asked for in
the mail below.
{{end}}
The review is kept under `{{.Review.Artifact}}` (`brief.json`, `angles/`, `findings.json`{{if .Review.JevRanked}}, `jev.json` with the judgment and eliminated findings{{end}}).
{{else}}
The review ran before this session: the change was sized `{{.Review.Size}}` and reviewed
from the {{join .Review.Angles ", "}} {{if eq (len .Review.Angles) 1}}angle{{else}}angles{{end}}, and the judge merged what they found into the list
below, {{if .Review.JevRanked}}by Jev probability, highest first, with eliminated findings removed{{else}}most severe first{{end}}. {{if .Review.Summary}}The brief's summary: {{.Review.Summary}}{{end}}
{{- if .Review.Skipped}}

Not reviewed — these angles did not look at the change, so their silence is not a
clean result:
{{range .Review.Skipped}}
- {{.}}
{{- end}}
{{- end}}
{{if .Review.Findings}}
{{.Review.Findings}}
{{else if .Review.JevRanked}}
Jev eliminated every finding; none remain to post.
{{else if .Review.Skipped}}
The judge's list is empty: the angles that reviewed the change found nothing to report,
and the ones not reviewed above found nothing because they did not look.
{{else}}
The judge's list is empty: no angle found anything to report.
{{end}}
The review is kept under `{{.Review.Artifact}}` (`brief.json`, `angles/`, `findings.json`{{if .Review.JevRanked}}, `jev.json` with the judgment and eliminated findings{{end}}).
{{end}}
{{- end}}
