{{if .Interrupted}}**The {{.Interrupted.Role}} session that ran for this issue before you was
{{if .Interrupted.Killed}}stopped{{else}}interrupted{{end}}{{if .Interrupted.Turns}} after {{count .Interrupted.Turns "turn"}}{{end}} and never reported an outcome.**
{{if .Interrupted.Transcript}}Its transcript is `{{.Interrupted.Transcript}}` — read it if you need to know
what it had already decided.{{else}}It stopped before it wrote a transcript.{{end}} The branch may carry work it never
reported: commits, edits it never committed, even a pull request it opened.
Inspect the working tree and the branch's commits before you write anything,
and carry on from what is there rather than starting over.

{{end -}}
