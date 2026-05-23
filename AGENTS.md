# AGENTS.md

Guidelines, guards, and conventions for agents working in this repository.
This file captures decisions made over the lifetime of the project; treat
them as established unless the user redirects.

## What this project is

`task2ci` is a single-binary Go tool that **fills in the steps** of a
GitHub Actions workflow template with task-driven steps derived from
`Taskfile.yaml`. The template lives at `.task2ci/workflows/<name>.yaml`,
the output at `.github/workflows/<name>.yaml`. The README is the user-facing
intro; this file is about how to work _on_ the codebase.

## Preflight (run this before saying a change is done)

The default Taskfile task runs the full preflight in parallel:

```sh
go tool task
```

That covers: `build`, `test`, `vet`, `lint` (golangci-lint), `yamllint`,
`markdownlint`, and `check` (workflow drift). All must be green. The tracked
pre-commit hook at `.githooks/pre-commit` runs the same thing.

## Generated files — do not hand-edit

- `.github/workflows/*.yaml` — generated. The header says so. Edits will be
  reverted on the next generation and `-check` will fail in CI.
  - `.gitattributes` marks them `linguist-generated=true`.
  - To change them: edit `.task2ci/workflows/<name>.yaml` (the template)
    and/or `Taskfile.yaml`, then run `go tool task generate`.

### Exception: hand-written workflows alongside generated ones

`.github/workflows/dependabot-regen.yaml` is hand-written. It triggers on
Dependabot PRs, regenerates the templated workflows, and pushes the result
back to the PR branch so `-check` passes after a version bump.

It lives in `.github/workflows/` because that's the only directory GitHub
Actions scans, and `.gitattributes` has an explicit `-linguist-generated`
override for this file. task2ci's normal generation doesn't touch it (the
generator only writes files for which a corresponding template exists under
`.task2ci/workflows/`).

If you add another hand-written workflow, follow the same pattern:
explicit `-linguist-generated` line in `.gitattributes`, top-of-file
comment explaining why it's not template-driven.

## Architecture

The flow is:

1. Walk `.task2ci/workflows/*.{yaml,yml}` for templates.
2. Parse `Taskfile.yaml`, find tasks annotated `# @ci: <tag>` (with optional
   `| <step name>` override), group by tag.
3. For each template, regex-substitute every `# @ci: <tag>` placeholder line
   with the rendered step block for that tag. Prepend the autogen header.
4. Warn on orphans (tag annotated but no template uses it, or vice versa).
5. Write to `.github/workflows/<name>.yaml` — or, in `-check` mode, fail if
   on-disk differs.

The substitution is **text-based** (regex on lines), not AST surgery. The
placeholder is a plain YAML comment at the indent of the surrounding
sequence items; we replace it with multi-line step text at the same indent.
This is deliberate — see "Why not AST" below.

### Why not AST

yaml.v3's `Node.Encode` strips `HeadComment` from values returned by
nested `MarshalYAML`. We hit this hard in the previous design and worked
around it by building nodes manually. Text-based substitution sidesteps
all of that: the template is treated as text with YAML-comment-shaped
splice markers, and the template author owns YAML structure entirely.

## Tool dependencies

External tooling is intentionally pinned via Go's `tool` directive in
`go.mod`. Invoke via `go tool <name>`:

| Tool | How |
|------|-----|
| `task` (go-task) | `go tool task` |
| `golangci-lint` | `go tool golangci-lint` |
| task2ci itself | `go tool task2ci` |

Don't `go install ...` system-wide. If a new linter/checker is needed,
add it with `go get -tool <module>` and use `go tool` to invoke it.

`yamllint` and `markdownlint-cli` are exceptions — they're not Go.
`yamllint` is assumed installed (system package). `markdownlint-cli` is
invoked via `npx --yes markdownlint-cli@0.41.0` because versions ≥ 0.42
require Node 20+ and the current dev environment is Node 18. If the
runtime Node version is upgraded, drop the pin.

## Code conventions

### Single file (`main.go`)

The whole tool lives in `main.go`. Don't split into packages unless the
file grows beyond what's reasonable to navigate. Tests are in `main_test.go`.

### Template substitution

- The placeholder regex is `(?m)^([ \t]*)# @ci:[ \t]*(\S+).*$\n?` — match
  consumes the trailing newline so empty substitutions cleanly remove the
  line.
- `renderStep` encodes via yaml.v3's struct encoder for one step (so values
  with `:`, leading dashes, YAML 1.1 booleans, etc., are quoted correctly),
  then re-indents and prefixes the first line with a `-` and a space.
- `renderBlock` joins steps with a blank line between consecutive items.
  No leading or trailing blank lines — the template's surrounding context
  owns outer spacing.
- `stripTrailingWhitespace` runs on the final output (belt-and-suspenders;
  most rendered lines are clean already, but yamllint has flagged trailing
  whitespace in past iterations and this is cheap to keep).

### Annotation syntax

```text
# @ci: <tag>                  # in Taskfile or template
# @ci: <tag> | <step name>    # Taskfile only — name override
```

The `|` was chosen over `:`, `->`, and quoted-string because it has the
simplest parser (single `strings.Cut`) and no conflicts with names that
contain colons, commas, quotes. Don't change the separator without
discussing.

Step name resolves in this order: annotation override → task `desc:` →
task name. Don't add comment-from-desc behavior back (it was removed
because the desc is more useful as the visible step name).

### Tool-dep detection (go-task)

`isToolDependency("github.com/go-task/task/v3/cmd/task")` decides
between `task <name>` and `go tool task <name>` in the generated step's
`run:` line. The parser walks `go.mod` line-by-line and supports both the
single-line form and the `tool ( ... )` block form, tolerating trailing
`// comments`. Don't replace with a regex.

## Tests

- Pure-helper tests (`hasToolDirective`, `parseAnnotation`, `taskDesc`,
  `renderStep`, `renderBlock`, `renderTemplate`, `stripTrailingWhitespace`,
  `outputPathFor`) call functions directly.
- Integration tests run the built binary against fixture directories under
  `t.TempDir()`. The binary is built once per `go test` invocation via a
  `sync.Once`-guarded helper (`buildBinary`).
- Integration coverage includes: generation end-to-end, `-check` happy
  path, `-check` drift detection, orphan-tag warning, orphan-placeholder
  warning, step-name override, multiple templates, `.yml`-extension
  acceptance, and the no-templates fatal.

## CLI surface

```text
task2ci                  # generate
task2ci -check           # fail if any drift or orphan tag/placeholder
task2ci -fix             # strip orphan placeholders from templates in place
task2ci -init            # scaffold a minimal template
task2ci -taskfile <path> # specify Taskfile path; repeatable
```

`-check` / `-fix` / `-init` are mutually exclusive. `-fix` only handles the
deterministic half of cleanup (orphan placeholders); orphan tags get a
copy-pasteable snippet in the warning instead of auto-generated templates,
because picking a workflow/job/runner for a new tag is an architectural
decision the user should make.

Adding more flags deserves a discussion about whether the behavior belongs
as a template construct (i.e., owned by the user) instead.

## Style preferences observed in this project

- **The template is the source of truth for workflow shape.** Don't
  re-introduce config fields that duplicate things expressible in the
  template (action versions, runner labels, job names, conditionals, etc.).
  That's exactly what the previous `.task2ci.yaml` design did wrong.
- **Generated workflows are formatted for human readability:** blank lines
  between consecutive inserted steps; preserve the template's surrounding
  spacing.
- **Warnings, not errors, for orphan tags/placeholders:** generation
  proceeds with what it has. The CLI exits 0; the user sees the warnings
  and fixes them.
- **One-off task in main, not a refactor:** the user has pushed back on
  premature splitting. Keep changes local unless explicitly invited to
  restructure.

## Gotchas

- **Emacs lock files** (`.#filename`) appear and disappear as files are
  opened in the IDE. They're symlinks to nonexistent targets and break
  tools that try to follow them. `.yamllint` ignores them; `.gitignore`
  excludes them. Don't be alarmed if `git status` shows them temporarily.
- **yamllint's `comments-indentation`** is disabled in `.yamllint` because
  our placeholder comments are deliberately indented to match list-item
  position, which the rule otherwise flags. Don't re-enable.
- **The autogen header is prepended after substitution**, so it ends up
  ABOVE the template's `---` document-start (if any). yamllint accepts
  this; actionlint accepts this. If a tool ever complains, swap to
  inserting the header _between_ a `---` line and the rest.

## When in doubt

- Run `go tool task`. If it passes, the change is likely fine.
- Run `./task2ci -check` to confirm self-CI is consistent.
- The user has been clear about wanting concise, focused changes — don't
  bundle drive-by refactors with feature work.
