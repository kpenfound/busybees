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

{{if eq .ChecksStatus "passed"}}CI is green; concentrate on the change itself.{{else}}Checks were still pending after `{{.ChecksTimeout}}`: judge the change on the code, and say in your note that CI had not reported when you reviewed.{{end}}
{{else}}
This repository reports no required checks: nothing was verified for you. Judge the
change on the code, and say so in your note.
{{end}}{{end}}
{{if .PreviousRounds}}
## Your feedback from previous rounds
{{- range .PreviousRounds}}
{{formatMail .}}
{{- end}}
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
{{if .Stages}}
## Review stages

Review in these stages, in this order. Each has its own focus and its own source of
truth, and each ends with a verdict of its own. Run every one of them: do not stop at
the first stage that finds something you will block on.
{{range .Stages}}
{{- if eq . "implementation"}}
### `implementation` — is it correct?

The diff is the source of truth. Error handling, edge cases, concurrency, security,
and the inputs and states the issue never mentioned. Weigh the tests too: a test that
would also pass on the code before the change pins nothing.
{{else if eq . "completeness"}}
### `completeness` — does it deliver the acceptance criteria?

The issue above is the source of truth. Take its acceptance criteria one at a time
and say which of them the diff meets. A criterion the pull request deviates from
deliberately, and says so, is a judgement call you can accept or reject; one it is
silent about is not delivered.
{{else if eq . "cleanliness"}}
### `cleanliness` — is it clear, small and free of dead code?

The diff is the source of truth. Needless abstraction, a helper with one caller, a
copy of something that already exists, commented-out code, and changes the issue did
not ask for.
{{else if eq . "style"}}
### `style` — does it follow the repository's conventions?

The repository's own conventions, its CLAUDE.md and what the linter reports are the
source of truth. Formatting and lint the tooling already enforces are the tooling's
job, not a review point: only raise what it does not catch.
{{else if eq . "product-fit"}}
### `product-fit` — does it fit the product?

{{if $.Parent}}The parent feature this work item belongs to — **#{{$.Parent.Number}}: {{$.Parent.Title}}** — is the
source of truth, together with the README and the docs.{{else}}This work item belongs to no feature, so the README and the docs are the only source
of truth; say that in the verdict.{{end}} The work item's own scope was settled before
it reached the developer, and this stage is not the place to re-open it: it is about
the change pulling the product somewhere the feature and the documentation do not go.
{{end}}
{{- end}}
End each stage with a verdict line of its own, in the stages' order:

    <stage>: pass — <one line>
    <stage>: fail — <one line>

Approve only when every stage passed.
{{end}}
## Instructions

The PR branch is checked out in your working directory. Read the diff and the issue,
{{if .Stages}}work through every stage above, {{end}}then either report `done` with
`status: approved` and a note, or send your feedback to the developer with `mail_send`
(`to: developer`, `pr: {{.PR.Number}}`, `issue: {{.Issue.Number}}`) and report `done`
with `status: changes-requested`.{{if .Stages}} One message, its points **grouped by
stage** in the stages' order, each group headed by that stage's verdict line.{{end}}
{{if ge .Round .MaxRounds}}
This is the final review round. If the PR is still not mergeable, request changes anyway;
the orchestrator will escalate it to a human.
{{end}}
Update your notes file before you finish.
