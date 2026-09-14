# Task: review pull request #{{.PR.Number}} (requested by a person)

A person asked for a review of this pull request, either by putting the
`{{.Labels.ReviewRequested}}` label on it or by setting the factory to review the
pull requests it did not write itself. It is not a pull request a developer session
opened for an issue: there is no issue behind it and no developer to send changes
back to, so your verdict goes on the pull request itself, as one GitHub review. Any
label has already been removed. This is one review pass; another comes from the
label going back on, or from a push to the branch.

## Pull request #{{.PR.Number}}: {{.PR.Title}}
{{.PR.URL}} — branch `{{.PR.HeadRefName}}` → `{{.PR.BaseRefName}}` · author: {{.PR.Author.Login}}

{{.PR.Body}}

## No issue, no acceptance criteria

This pull request closes no issue the factory tracks, and nobody wrote acceptance
criteria for it: the review below judged the change against what its description says
it does and against the repository's own conventions. Do not invent criteria the
description does not state when you decide the verdict.

## Who you are on GitHub
{{if .ActsAs}}
The factory acts as `{{.ActsAs}}` on GitHub. {{if eqFold .ActsAs .PR.Author.Login}}That is this pull request's author, and
GitHub refuses an approval from a pull request's own author: where you would approve,
submit a `comment` review instead, and say in it that nothing needs fixing and why it is
not an approval.{{else}}The pull request's author is `{{.PR.Author.Login}}`, so an approval
is accepted.{{end}}
{{else}}
The factory has no GitHub account of its own: it acts as whoever `gh` is signed in as,
in practice one of the people it works for. Run `gh api user --jq .login` before you
submit. When that login is `{{.PR.Author.Login}}`, this pull request's author, GitHub
refuses an approval: where you would approve, submit a `comment` review instead, and
say in it that nothing needs fixing and why it is not an approval.
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
{{template "findings" .}}
## Instructions

The pull request's head branch is checked out in your working directory, or the
default branch when the remote does not have it. Read the pull request with `pr_view`,
then submit the findings above as exactly one GitHub review with `submit_review`
(`number: {{.PR.Number}}`): `event: approve` when nothing needs fixing,
`event: request-changes` when something does, `event: comment` in place of `approve`
when you are the author (see above). The body is the whole review: the verdict line,
then every finding as it is, nothing dropped and nothing added. The
`<!-- bees:reviewer -->` marker is appended for you. Then report `done` with
`status: approved` (after an approval, or a comment in its place) or
`status: changes-requested`, and a one-line note.

One review, nothing else on the pull request: do not comment on it as well, and send no
mail — there is no developer on this pull request.

Update your notes (`notes_write`) before you finish.
