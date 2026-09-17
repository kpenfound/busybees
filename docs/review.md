# Reviewing a pull request: `bees review`

`bees review` reviews one GitHub pull request, any pull request your `gh`
login can read. It runs without a factory: it reads no `bees.toml`, no state
directory and no mailbox. Several read-only agent sessions review the change,
each from one angle, bees merges what they found into one list, and you
decide about each finding before the review ends.

```sh
cd ~/src/widgets
bees review 7
```

The commands and their flags are in the
[CLI reference](cli.md#reviewing-a-pull-request). This page covers what a
review does, where it keeps what it found, and the two files that configure
it.

## How a review runs

```
context  ->  brief  ->  angles  ->  findings  ->  triage  ->  end
```

1. **Context.** bees gathers the pull request's context from the sources
   `context.toml` enables: the diff, the pull request's conversation, the
   issues it links, the project's style files and the callers of what it
   changed. [Generated files](#generated-files) are taken out of the diff
   before anything reads it. A source that cannot read something records
   what it missed, and the review goes on.
2. **Brief.** One session, the distiller, reads everything gathered and
   writes the brief: what the change does, its size (`xs`, `s`, `m`, `l` or
   `xl`, judged from how many files and lines it touches and which parts of
   the project those are), its acceptance criteria with the issue or text
   each came from, the style rules that apply to it with the file each is
   written in, the parts of the project it touches, the generated files
   taken out of the diff with the lines each added and removed, and what
   the sources could not read.
3. **Angles.** One session per [angle](#angles) the brief's size calls for
   and `context.toml` enables reads the brief and the diff, and answers with
   findings. The angles run at the same time, and at a terminal the
   command shows each one's progress while they do. An angle that
   fails is skipped; the review stops only when every angle fails.
4. **Findings.** bees merges the angles' answers into one list, in code and
   not in a session (see [Findings](#findings)), then applies the rules in
   your [reviewer notes](#reviewer-notes): a finding you dismissed before is
   dropped or ranked down.
5. **Triage.** You take each finding in turn and select, dismiss, defer or
   ask about it. With `--agent`, an agent session does it instead: see
   [factory mode](cli.md#factory-mode).
6. **End.** What you selected is posted as one review, printed as a
   report, or discarded: see [How a review ends](#how-a-review-ends).

Every session is read-only. A `claude` session may use `Read`, `Grep`,
`Glob`, `LS` and `NotebookRead` and nothing else; a `codex` session runs in
Codex's read-only sandbox. No session runs the tests, builds the change or
writes to the repository.

As the context is gathered, bees checks out the pull request's head, in a
container: an Alpine image with git, built the first time and kept, clones
`refs/pull/<number>/head` of the pull request's repository over HTTPS into
the review's `checkout/` directory, fetches the base branch beside it, and
exits. That is the head as it is on GitHub, whatever your working tree has
checked out, and a fork's pull request as much as one from a branch of the
repository. The `diff` source reads the diff from that checkout, with your
machine's git, as the head against its merge base with the base branch, the
diff GitHub shows: GitHub's own diff is refused for a pull request that
changes more than 300 files, and this one is not. When the head and the base
branch have no merge base, the diff is against the base branch's tip and the
review lists that among the context it skipped. The angle sessions run in
that same directory, on your machine, with the same read-only tools; nothing
runs in the container after the clone. `github.token` authenticates the
clone when it is set, handed to the container as an environment variable;
without it the clone is anonymous, which a private repository refuses.

The checkout needs `docker`. Without it, or when the image does not build
or the clone fails, the review says so and goes on: the diff is read with
`gh pr diff` instead, and the angles run in the current directory when it
is a checkout of the pull request's repository (its `origin` remote points
at it), and in the empty `scratch/` directory of the review anywhere else.
A diff that cannot be read either way is reported as not gathered, like
any other source, and the review goes on without it.

The other sessions run where the angles would without that checkout: the
distiller in the current directory when it is a checkout of the repository
and in an empty temporary directory anywhere else, and the triage sessions
of factory mode in the current directory or in `scratch/`. The sources that
read files (`style_files`, `callers` and the project's own) read the current
directory when it is that checkout and gather nothing anywhere else.

### Context sources

| Source | What it gathers |
|---|---|
| `diff` | The pull request's diff without its generated files, read from the checkout made for the review, or with `gh pr diff` when there is none |
| `pr_body` | The title and body, then every comment and submitted review, oldest first |
| `linked_issues` | The issues the pull request closes, then the others its body mentions, at most 20 |
| `style_files` | `AGENTS.md`, `CLAUDE.md`, `CONTRIBUTING.md`, `STYLE.md` and `.editorconfig`, plus the files `style_sources` names |
| `callers` | The lines outside the changed files that name a function the diff declares: the first 20 functions, 40 lines each |

Every one of them runs unless `context.toml` turns it off, and a project adds
sources of its own that gather the files they name. See
[`context.toml`](#contexttoml).

### Generated files

A generated file is taken out of the diff before the distiller and the
angles read it, so a change that regenerates a protobuf binding is sized
and reviewed by what was written by hand. A file is generated when any of
these says so:

- a line `// Code generated ... DO NOT EDIT.` in its first 64 lines (and
  its first 16 KiB), on the side the change leaves it (the old side for a
  file the change deleted);
- `.gitattributes` in the checkout marks it `linguist-generated`, as git
  reads the file (`git check-attr`);
- a pattern in `context.toml`'s `generated` list matches it.

The brief lists every file taken out, with the lines the change added and
removed there and which of the three said it was generated, and a finding
an angle anchors in one is dropped. The first two checks read the checkout
of the pull request's head; without one (no `docker`, or a clone that
failed) only the patterns are checked, and the brief says so under what
was not gathered. Triage still shows the whole diff.

### Angles

| Angle | What it looks for |
|---|---|
| `quick_general` | One light pass over the change as a whole, for what a careful reader catches on one read |
| `general` | A thorough pass over the change as a whole: wrong logic, a dropped error, duplicated code, and a line that breaks a rule the project wrote down |
| `docs` | A comment or doc comment the change made false, and prose the change wrote that the code does not bear out |
| `test_coverage` | Behaviour with no test on it, a test that would pass with the change undone, and a document the change made false |
| `acceptance_criteria` | A criterion from the brief the change does not meet, or meets only in part, and a behaviour change nothing asked for |
| `side_effects` | A caller the change did not update, an invariant it no longer keeps, and a claim elsewhere in the repository it made false |

The brief assigns the size automatically, from the scope and the risk of the
diff, and a review runs the angles that size calls for:

| Size | Angles |
|---|---|
| `xs`, `s` | `quick_general`, `docs` |
| `m`, `l` | `general`, `docs`, `test_coverage`, `acceptance_criteria` |
| `xl` | `general`, `docs`, `test_coverage`, `acceptance_criteria`, `side_effects` |

A project's `angles.<angle>` entry in
[`context.toml`](#contexttoml) still turns an angle off entirely; the size
only narrows what is left on, and never re-enables an angle the project
turned off.

An angle session is also told the rules in your reviewer notes about its
angle, so it reports less of what you have dismissed before.

## Findings

A finding is one problem, as the review keeps it in `findings.json`:

```json
{
  "id": "1a2b3c4d",
  "angle": "test_coverage",
  "session_id": "5f0c...",
  "category": "missing test",
  "severity": "high",
  "file": "internal/review/gather.go",
  "lines": [12, 14],
  "side": "new",
  "title": "Gather has no test for a source that cannot read",
  "body": "The acceptance criterion says a source that cannot read ...",
  "evidence": "gather_test.go covers only sources that succeed",
  "sources": ["#566"]
}
```

`file`, `lines` and `side` are absent on a finding about the change as a
whole. `side` is `new` for the file as the change leaves it and `old` for
lines the change removed. `suggestion`, present when the angle could write
it, is the text that would replace the lines.

The merge is the same for every review:

- **Severity** is one of `info`, `low`, `medium` and `high`. The words a
  session uses instead are mapped onto them (`critical` and `blocker` are
  `high`, `nit` is `low`, `fyi` is `info`), and a word bees does not know is
  `medium`. A category `context.toml` pins takes the pinned severity, and a
  category pinned `off` is dropped.
- **The same problem from two angles** is kept once: two findings on the
  same file and side with overlapping lines, in the same category or worded
  alike, or two findings about the whole change worded alike. The more
  severe one is kept, with the other angle's name in `also_from`.
- **Order** is most severe first.
- **The id** is a hash of the file, side, lines and title, so a finding
  keeps its id when the list is merged again.

## Reviewer notes

Your reviewer notes are one markdown file, `reviewer-notes.md` next to your
`config.toml` unless `notes_path` says otherwise. Every finding you dismiss
during triage appends a line to it, with the reason you gave:

```
- [acme/widgets] [general] [naming] receiver names are short here
```

`bees review consolidate` turns the dismissals that repeat into rules,
written between two markers:

```
<!-- bees:review:rules -->
- [acme/widgets] [general] [naming] drop: receiver names are short here (3 dismissals)
- [*] [test_coverage] [*] downrank: generated files carry no tests (2 dismissals)
<!-- /bees:review:rules -->
```

A pattern dismissed twice becomes a `downrank` rule, three times or more a
`drop` rule. Each review applies the rules to its list before triage: a
matching finding is dropped, or ranked down one severity. `*` matches any
repository, angle or category, and a rule with no text after the action
matches every finding in its repository, angle and category. You can edit a
rule, and consolidation keeps your wording and changes only its count. See
[`bees review consolidate`](cli.md#bees-review-consolidate---notes-path---dry-run).

## Triage

Triage takes the findings one at a time, most severe first:

| Action | What it does |
|---|---|
| select | The finding goes into the review's output, with its text as written or as you edited it |
| dismiss | The finding is left out, and the reason is appended to your reviewer notes |
| defer | The finding is left out of this review, and nothing is recorded |
| ask | The angle that found it is asked a question, in its own session; a finding the answer turns up joins the queue |

Every decision is written into the review's directory as it is taken, and
the latest select, dismiss or defer on a finding is the one in force.
`bees review triage <pr>` reopens the latest review of a pull request and
offers what is still undecided. See
[`bees review triage`](cli.md#bees-review-triage-pr---post-mode----report---config-path)
for the keys, and [factory mode](cli.md#factory-mode) for triage by an agent
(`--agent`, `--instructions`).

At a terminal, triage opens as a full-screen view beside the diff, the same
keys pressed without return. `--no-tui`, or a stdout that is not a terminal,
keeps the console described there instead.

## How a review ends

| End | What happens |
|---|---|
| `approve` | The selected findings are posted as review comments, and the pull request is approved |
| `comment` | The same comments, as a comment-only review |
| `reject` | The same comments, as a review that requests changes |
| `report` | The selected findings are printed as markdown, and nothing is posted |
| `discard` | Nothing is posted and nothing is printed |

`--post approve|comment|reject` or `--report` on the command line chooses.
With neither, the `output` key of `config.toml` does, and its default, `ask`,
asks you at the end. With `--agent` and `ask`, the agent chooses.

The three that post submit one review in one `gh api` call. A finding whose
lines are all in the diff is a comment on them, with its suggestion as a
suggestion block on the new side. Any other finding goes into the review's
summary, with where it points. `comment` and `reject` with nothing selected
are refused; `approve` with nothing selected approves without comments.

## The review directory

Every review is a directory under `storage_path`, written as the review
goes:

```
~/.config/bees/reviews/acme/widgets/7/20260910-150405/
  brief.json            the brief, and the distiller's session id
  angles/<angle>.json   each angle's session id, directory and answer
  checkout/             the pull request's head, cloned for the diff and
                        the angles; also holds diff.patch, for every
                        angle to read
  scratch/              where the angles and triage ran without a checkout;
                        also holds diff.patch, on the same terms
  findings.json         the merged list, and what your notes hid from it
  triage.json           every triage decision, in order
```

The directory name is when the review started, in UTC. A review that stopped
partway keeps what it reached; one that stopped before the brief was
written leaves no directory. `bees review triage` reopens the newest
directory of the pull request, and an ask resumes the angle's session from
its file under `angles/`. When the angles ran in your machine's own
checkout instead of one made for the review, diff.patch is not written
there; an angle reads the code as the change leaves it in that case.

## Configuration

`bees review` reads two files, and neither is `bees.toml`. Both are
optional: a missing file loads as the defaults. An unknown key, or a value
outside the ones listed, fails the command with an error naming the key.

### `config.toml`

Your own settings: `~/.config/bees/config.toml`, or
`$XDG_CONFIG_HOME/bees/config.toml` when `XDG_CONFIG_HOME` is an absolute
path. `--config` reads another file.

```toml
provider = "claude"
model = "opus"
brief_model = "sonnet"
notes_path = "reviewer-notes.md"
storage_path = "reviews"
output = "ask"

[angles]
xs = ["quick_general"]

[angle_models]
docs = "haiku"

[github]
token = "$REVIEW_GH_TOKEN"
```

| Key | Type | Default | Meaning |
|---|---|---|---|
| `provider` | string | `"claude"` | The agent every session runs as: `claude` or `codex` |
| `model` | string | `"opus"` | The model every session uses unless a key below names another |
| `brief_model` | string | `model` | The model of the distiller session that writes the brief |
| `angle_models.<angle>` | string | `model` | The model of that angle's session |
| `judge_model` | string | none | Accepted and checked, and has no effect: the judge is not a session |
| `angles.<size>` | list | the built-in list | The angles a change of that size is reviewed from |
| `notes_path` | path | `"reviewer-notes.md"` | Your reviewer notes |
| `storage_path` | path | `"reviews"` | The directory review directories are created in |
| `output` | string | `"ask"` | How a review ends without `--post` or `--report`: `ask`, `approve`, `comment`, `reject`, `report` or `discard` |
| `github.token` | string | none | The token the review's `gh` calls use; without it, your own `gh` login |

A path is absolute, starts with `~`, or is relative to the directory
`config.toml` is in, which puts the defaults at
`~/.config/bees/reviewer-notes.md` and `~/.config/bees/reviews/`.

`angles.<size>` replaces the list [Angles](#angles) gives that size;
`<size>` is `xs`, `s`, `m`, `l` or `xl`, and the list names at least one
angle. `context.toml` still turns angles off on top of it. Every session
runs as `provider`: `brief_model` and `angle_models` change the model only.
`judge_model` gives the file the shape of `roles.reviewer` in `bees.toml`,
where the judge is a session; in `bees review` the judge is code and uses
no model.

`github.token` takes a `$VAR` or `${VAR}` reference, expanded from the
environment, so the secret stays out of the file. A reference to a variable
that is not set fails the command.

### `context.toml`

The project's settings, committed to the repository under review. bees
looks for it in the current directory and then each parent, and only when
the current directory is a checkout of the pull request's repository.
Without one, a review runs every angle its size calls for and every
built-in source, and pins no category.

```toml
style_sources = ["docs/style/*.md"]
generated = ["*.pb.go", "vendor/**"]

[angles]
side_effects = false

[categories]
naming = "low"
"missing test" = "high"
typo = "off"

[[context_sources]]
name = "callers"
enabled = false

[[context_sources]]
name = "architecture"
files = ["docs/architecture.md", "docs/adr/*.md"]
```

| Key | Type | Default | Meaning |
|---|---|---|---|
| `angles.<angle>` | bool | `true` | `false` turns the angle off. `<angle>` is `quick_general`, `general`, `docs`, `test_coverage`, `acceptance_criteria` or `side_effects` |
| `style_sources` | list of paths or globs | `[]` | Style documents the `style_files` source gathers on top of the built-in names |
| `generated` | list of paths or globs | `[]` | Files taken out of the diff as [generated](#generated-files), on top of the ones a header or `.gitattributes` marks |
| `categories.<category>` | string | none | Pins every finding in the category to `info`, `low`, `medium` or `high`, or drops them with `off` |
| `context_sources` | list of tables | none | Turns a built-in source off, or adds a source of the project's own |

A `[[context_sources]]` entry has a `name`, and:

- For a built-in source (`diff`, `pr_body`, `linked_issues`, `style_files`,
  `callers`), `enabled = false` turns it off. It takes no `files`.
- For a name of your own, `files` lists the paths or globs to gather, and
  `enabled = false` turns it off without deleting the entry. A pattern that
  matches nothing is reported in the brief as not gathered.

Paths and globs are relative to the directory `context.toml` is in and must
stay inside it. A `generated` pattern is relative to the repository root, as
the diff names files: `*`, `?` and `[...]` match within one path segment and
`**` any number of them, a pattern with no `/` matches a file's name in any
directory, and a pattern that matches a directory matches every file under
it.

A category is the angle's own word for a kind of problem, lowercased:
triage shows it next to the severity, and it is the third field of a
dismissal in your reviewer notes. A pin matches it whatever case
`context.toml` writes it in.
