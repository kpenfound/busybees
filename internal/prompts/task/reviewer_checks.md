# Task: required checks failed on pull request #{{.PR.Number}} (fix round {{.Round}} of {{.MaxRounds}})

You approved this pull request, but its required checks did not pass, so it cannot be
merged yet. Find out why and hand the developer a precise fix request.

## Pull request #{{.PR.Number}}: {{.PR.Title}}
{{.PR.URL}} — branch `{{.PR.HeadRefName}}` → `{{.PR.BaseRefName}}`

## Issue #{{.Issue.Number}}: {{.Issue.Title}}
{{.Issue.URL}}

## Failing checks
{{range .FailedChecks}}
- **{{.Name}}**{{if .Workflow}} ({{.Workflow}}){{end}} — {{.Bucket}}{{if .Description}}: {{.Description}}{{end}}{{if .Link}}
  {{.Link}}{{end}}
{{- end}}

## Your notes

{{if .Notes}}{{.Notes}}{{else}}_Empty._{{end}}

## Instructions

1. Find out what failed. Checks can come from any CI system; do not assume GitHub
   Actions. Start from each check's details link and description above and from
   `gh pr checks {{.PR.Number}} -R {{.Project.Repo}}`; the repository's documentation
   and your notes may explain where its CI runs and how to read it. If the link is a
   GitHub Actions run, `gh run view <run-id> -R {{.Project.Repo}} --log-failed` prints the
   failing steps; for other systems fetch the page or use the tooling the repository
   documents. If logs are not reachable, reproduce the failure locally on this branch
   with the repository's documented build/test commands — that is usually faster anyway.
   Record what you learn about reading this project's CI in your notes.
2. Distil the **main error message** and its cause. Ignore noise; the developer needs the
   one thing to fix, with file/line where possible, plus the command that reproduces it.
3. Send it to the developer:
   `bees mail send --to developer --pr {{.PR.Number}} --issue {{.Issue.Number}} --subject "Required check failed: <name>" --body-file <file>`
   and report `bees done changes-requested`. The developer will push a fix and the checks
   will run again.
4. If the failure is clearly unrelated to the change (infrastructure, flakiness) and the
   CI system lets you re-run it (for GitHub Actions: `gh run rerun <run-id> --failed -R {{.Project.Repo}}`),
   re-run it and report `bees done approved` so the orchestrator waits for the checks
   again. Do this at most once. If you cannot re-run it yourself, tell the developer to
   re-trigger it (for example with an empty commit) via the same mail command.
{{if ge .Round .MaxRounds}}
This is the last fix round; if the checks fail again the orchestrator escalates to a human.
{{end}}
Update your notes file before you finish.
