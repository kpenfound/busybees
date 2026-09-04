{{if .Stages}}
## Review stages

Review in these stages, in this order. Each has its own focus and its own source of
truth, and each ends with a verdict of its own. Run every one of them: do not stop at
the first stage that finds something you will block on.
{{range .Stages}}
{{- if eq . "implementation"}}
### `implementation` — is it correct?

The diff is the source of truth. Error handling, edge cases, concurrency, security,
and the inputs and states the {{if eq $.Mode "requested"}}description{{else}}issue{{end}} never mentioned. Weigh the tests too: a test that
would also pass on the code before the change pins nothing.
{{else if eq . "completeness"}}
### `completeness` — does it deliver the acceptance criteria?

{{if eq $.Mode "requested"}}The pull request's description is the source of truth: there is no issue and no
acceptance criteria. Take what the description says the change does, one claim at a
time, and say which of them the diff delivers. Do not invent criteria the description
does not state.{{else}}The issue above is the source of truth. Take its acceptance criteria one at a time
and say which of them the diff meets. A criterion the pull request deviates from
deliberately, and says so, is a judgement call you can accept or reject; one it is
silent about is not delivered.{{end}}
{{else if eq . "cleanliness"}}
### `cleanliness` — is it clear, small and free of dead code?

The diff is the source of truth. Needless abstraction, a helper with one caller, a
copy of something that already exists, commented-out code, and changes the {{if eq $.Mode "requested"}}description{{else}}issue{{end}} did
not ask for.
{{else if eq . "style"}}
### `style` — does it follow the repository's conventions?

The repository's own conventions, its CLAUDE.md and what the linter reports are the
source of truth. Formatting and lint the tooling already enforces are the tooling's
job, not a review point: only raise what it does not catch.
{{else if eq . "product-fit"}}
### `product-fit` — does it fit the product?

{{if eq $.Mode "requested"}}This pull request belongs to no feature, so the README and the docs are the only
source of truth; say that in the verdict. This stage is about the change pulling the
product somewhere the documentation does not go, not about whether the change was
worth making: that was the author's call.{{else}}{{if $.Parent}}The parent feature this work item belongs to — **#{{$.Parent.Number}}: {{$.Parent.Title}}** — is the
source of truth, together with the README and the docs.{{else}}This work item belongs to no feature, so the README and the docs are the only source
of truth; say that in the verdict.{{end}} The work item's own scope was settled before
it reached the developer, and this stage is not the place to re-open it: it is about
the change pulling the product somewhere the feature and the documentation do not go.{{end}}
{{end}}
{{- end}}
End each stage with a verdict line of its own, in the stages' order:

    <stage>: pass — <one line>
    <stage>: fail — <one line>

Approve only when every stage passed.
{{end -}}
