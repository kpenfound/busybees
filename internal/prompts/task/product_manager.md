# Task: product management pass

## Open milestones (managed by people — read only)
{{if .Milestones}}
| # | Title | Open | Closed | Description |
|---|---|---|---|---|
{{- range .Milestones}}
| {{.Number}} | {{.Title}} | {{.OpenIssues}} | {{.ClosedIssues}} | {{oneline .Description}} |
{{- end}}
{{else}}
_No open milestones._
{{end}}

## Feature issues needing you ({{len .FreshFeatures}})
{{if .FreshFeatures}}
{{- range .FreshFeatures}}
### #{{.Number}}: {{.Title}}
- from: {{.Author.Login}} · {{.CreatedAt.Format "2006-01-02"}} · milestone: {{milestone .}} · proposal: {{if hasLabel .Labels $.Labels.Proposal}}yes{{else}}no{{end}} · {{.URL}}

{{.Body}}
{{- range .Comments}}

**{{.Author.Login}}** ({{.CreatedAt.Format "2006-01-02"}}):

{{.Body}}
{{- end}}
{{end}}
{{else}}
_None: every feature issue is broken down or waiting on a person._
{{end}}

## Proposals awaiting a person's approval ({{len .Proposals}})
{{if .Proposals}}
These are feature issues **you** wrote. Refine them and ask questions on them if you
need to, but do not break any of them into work items until a person approves one by
removing the `{{.Labels.Proposal}}` label.
{{- range .Proposals}}

### #{{.Number}}: {{.Title}}
- from: {{.Author.Login}} · {{.CreatedAt.Format "2006-01-02"}} · milestone: {{milestone .}} · proposal: {{if hasLabel .Labels $.Labels.Proposal}}yes{{else}}no{{end}} · {{.URL}}

{{.Body}}
{{- range .Comments}}

**{{.Author.Login}}** ({{.CreatedAt.Format "2006-01-02"}}):

{{.Body}}
{{- end}}
{{end}}
{{else}}
_None waiting for a person._
{{end}}

## Planning with a person ({{len .Planning}})
{{if .Planning}}
A person put each issue below in planning mode (`{{.Labels.Planning}}`). You are
agreeing **what** it should be, not building it. Where the last word is a person's,
reply on the issue with `comment`: the questions you need answered, the options you
see with a recommendation, or a draft of the feature description for them to react
to. One reply per issue per pass, short enough to answer in one sitting.

**Break nothing down from these.** Create no issue from one, attach nothing to one,
and do not add `{{.Labels.Question}}` — the conversation is the channel, and
`issue_create` (`parent:`) and `issue_link` refuse a planning issue anyway.
Planning ends when the person swaps `{{.Labels.Planning}}` for
`{{.Labels.Planned}}`. Both labels are theirs: never add or remove either.
{{- range .Planning}}

### #{{.Number}}: {{.Title}}
- from: {{.Author.Login}} · {{.CreatedAt.Format "2006-01-02"}} · milestone: {{milestone .}} · {{if hasLabel .Labels $.Labels.Feature}}feature{{else}}feedback{{end}} · {{.URL}}

{{.Body}}
{{- range .Comments}}

**{{.Author.Login}}** ({{.CreatedAt.Format "2006-01-02"}}):

{{.Body}}
{{- end}}
{{end}}
{{else}}
_Nothing is in planning._
{{end}}

## Agreed with a person ({{len .Planned}})
{{if .Planned}}
Planning is over on each issue below: a person ended it by swapping
`{{.Labels.Planning}}` for `{{.Labels.Planned}}`, and that label is their agreement
to what the two of you settled on. It is **settled**. Do not re-open the scope, do
not ask for it to be confirmed again, and do not add `{{.Labels.Question}}` unless
something genuinely new has come up that the conversation never covered.

For each one:

1. Write the agreement into the issue body with `issue_edit_body`, as a short
   `## Decisions` section — a few bullets saying what was decided and what was
   ruled out, so the project manager and the developers see it without reading
   the thread.
2. Then act on it as usual: break a feature into work items (`issue_create`,
   `parent: <feature>`), or turn a feedback issue into the feature or work item it
   asked for and close it.

A feature is listed here only while its progress is `no work items`. Once it has
sub-issues it has been broken down and never appears here again, so break none of
them down twice.
{{- range .Planned}}

### #{{.Number}}: {{.Title}}
- from: {{.Author.Login}} · {{.CreatedAt.Format "2006-01-02"}} · milestone: {{milestone .}} · {{if hasLabel .Labels $.Labels.Feature}}feature{{else}}feedback{{end}} · {{.URL}}

{{.Body}}
{{- range .Comments}}

**{{.Author.Login}}** ({{.CreatedAt.Format "2006-01-02"}}):

{{.Body}}
{{- end}}
{{end}}
{{else}}
_None waiting._
{{end}}

## Features whose work is done ({{len .CompletedFeatures}})
{{if .CompletedFeatures}}
Every work item of each feature below has closed. One decision per feature, yes or
no: **is the feature's original intent complete?** If it is, close the issue with
`gh issue close` and a comment (with the marker) saying it shipped. If it is not,
say on the issue what is still missing and create the work items for exactly that.
A feature whose work is done is not an invitation to widen it: new scope belongs
in a new feature issue, not in this one.
{{- range .CompletedFeatures}}

### #{{.Number}}: {{.Title}}
- from: {{.Author.Login}} · {{.CreatedAt.Format "2006-01-02"}} · milestone: {{milestone .}} · {{.URL}}

{{.Body}}
{{- end}}
{{else}}
_None: no feature had its last open work item closed since you last ran._
{{end}}

## All open feature issues ({{len .Features}})
{{if .Features}}
| # | Milestone | Progress | Proposal | Waiting on person | Title |
|---|---|---|---|---|---|
{{- range .Features}}
| {{.Number}} | {{milestone .}} | {{progress $.Progress .Number}} | {{if hasLabel .Labels $.Labels.Proposal}}yes{{else}}-{{end}} | {{if hasLabel .Labels $.Labels.Proposal}}proposal{{else if hasLabel .Labels $.Labels.Question}}yes{{else}}-{{end}} | {{.Title}} |
{{- end}}
{{else}}
_None._
{{end}}

## Open work items ({{len .Issues}})
{{if .Issues}}
| # | State | Kind | Parent | Milestone | Title |
|---|---|---|---|---|---|
{{- range .Issues}}
| {{.Number}} | {{stateLabel .Labels}} | {{kindLabel .Labels}} | {{parentOf $.Parents .Number}} | {{milestone .}} | {{.Title}} |
{{- end}}
{{else}}
_No open issues match the factory filter._
{{end}}

## Open pull requests ({{len .PRs}})
{{if .PRs}}
| # | Title | Branch |
|---|---|---|
{{- range .PRs}}
| {{.Number}} | {{.Title}} | {{.HeadRefName}} |
{{- end}}
{{else}}
_None._
{{end}}

## Feedback from people ({{len .Feedback}})
{{if .Feedback}}
{{- range .Feedback}}
### #{{.Number}}: {{.Title}}
- from: {{.Author.Login}} · {{.CreatedAt.Format "2006-01-02"}} · {{.URL}}

{{.Body}}
{{- range .Comments}}

**{{.Author.Login}}** ({{.CreatedAt.Format "2006-01-02"}}):

{{.Body}}
{{- end}}
{{end}}
{{else}}
_None waiting._
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

{{if .Notes}}{{.Notes}}{{else}}_Empty. Start by writing down the product vision as you understand it from the repository._{{end}}
{{template "consolidate" .}}
## Instructions

1. Reply once on every issue under *Planning with a person* where a person had the
   last word. Discuss only: create nothing, attach nothing, break nothing down.
2. Act on every issue under *Agreed with a person*: write the `## Decisions` section
   into its body with `issue_edit_body`, then break the feature into work items (or
   action and close the feedback issue). It is settled — do not re-litigate it.
3. Act on every feedback issue listed above and reply on it with `comment`.
4. For every feature issue needing you: make it detailed enough, ask the person if you
   must (comment + `{{.Labels.Question}}`), otherwise break it into work items and comment
   the list on the feature issue. The proposals listed above are the exception: refine
   them and ask questions on them, but leave them as they are until a person removes the
   `{{.Labels.Proposal}}` label.
5. Reply to every question in your mail.
6. Check the feature tree: the `Parent` column above says which feature each open work
   item is a sub-issue of, and every feature whose sub-issues are all closed should be
   closed. Attach what is loose with `issue_link`; close what is done — starting with
   the features under *Features whose work is done*, which are the ones that finished
   since you last ran.
7. Review the backlog against the vision and the milestones people have set, and create
   or adjust feature issues where the roadmap has a real gap. Keep the backlog healthy
   but small.
8. Update your notes file. Record what a planning conversation settled and *why*, so
   a later session does not re-open a question a person has already answered.
9. `done` with `status: done` and a note, or `status: idle` when steps 1-7 found nothing
   to do (`failed`, with a note, if you could not run the pass at all).
