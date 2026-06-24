---
title: task2ci
layout: default
---

<div class="callout-caution" markdown="1">
<span class="callout-title">⚠ Caution — experimental</span>
<p>
This project is still very much experimental. The CLI, template format, and
generated workflow shape may change without notice. Pin a specific commit
if you depend on it, and don't be surprised by breaking changes between
versions until 1.0.
</p><p>
This experiment didn't lead to the developer experience I had in
mind. The project is archived and won't be developed further. It
gave me a lot of insight, though, so it was a worthwhile experiment!
</p>
</div>

Generate GitHub Actions workflows from your `Taskfile.yaml`, so the commands you
run locally are the same ones CI runs — guaranteed, not by hand.

## What it does

You write a workflow **template** under `.task2ci/workflows/<name>.yaml` —
plain GitHub Actions YAML with `# @ci: <tag>` placeholder comments where you
want task-driven steps spliced in. You mark the [go-task](https://taskfile.dev/) tasks that should
fill those slots with the same `# @ci: <tag>` annotation in `Taskfile.yaml`.

Run `task2ci` and each template is rendered to `.github/workflows/<name>.yaml`
with the matching task steps in place. A `task2ci -check` mode fails CI if the
on-disk workflow has drifted from the template/Taskfile, so the two can't
quietly diverge.

## Why

Any project that uses [go-task](https://taskfile.dev/) ends up with two
parallel lists of commands:

- `Taskfile.yaml` — what developers run locally (e.g. `task test`, `task lint`)
- `.github/workflows/ci.yaml` — what CI runs (the same commands, restated
  by hand in YAML)

These drift. Someone adds a new check to the Taskfile but forgets the
workflow, or vice versa, and "works on my machine" creeps in.

`task2ci` keeps the **Taskfile** as the source of truth for _what_ runs, and
keeps the **template** as the source of truth for _the rest_ of the workflow
(triggers, runner image, setup steps, conditionals, environments). The
template is plain GitHub Actions YAML, so you get full validation from
yaml-language-server, actionlint, and any other GHA tooling — task2ci itself
does not model setup steps, runners, or any other workflow structure.

The tool is language-agnostic; the Quick start below uses a Go example
because that's the dogfood, but the only Go-specific behavior is an
[optional optimization](#how-it-adapts-to-your-toolchain) that uses
`go tool task` when `go-task` is registered as a Go tool dependency.

## Quick start

In a project that already has a `Taskfile.yaml`:

```sh
# Install task2ci. Go projects can register it as a tool dep:
go get -tool arnested.dk/go/task2ci

# Anywhere else, install the binary with `go install` or download a release
# build, then make sure `task2ci` is on the runner's PATH.

# Scaffold a starter template:
task2ci -init
```

The starter template at `.task2ci/workflows/ci.yaml` looks like this — it's
plain GitHub Actions YAML, so swap the setup steps for whatever your project
needs (setup-node, setup-python, system packages, …):

```yaml
---
name: ci
on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4

      # Set up the toolchain your tasks need. This Go example uses
      # actions/setup-go; replace with what your project needs.
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod

      - name: Check generated CI is up to date
        run: go tool task2ci -check
      # @ci: test
```

Annotate the tasks you want spliced in:

```yaml
# Taskfile.yaml
---
version: '3'

tasks:
  # @ci: test
  test:
    desc: Run unit tests
    cmd: go test ./...

  # @ci: test
  vet:
    cmd: go vet ./...

  # local-only — no annotation
  tidy:
    cmd: go mod tidy
```

Generate:

```sh
task2ci    # or `go tool task2ci` if you registered it as a tool dep
```

You get `.github/workflows/ci.yaml`, identical to the template except the
`# @ci: test` line is replaced by the two annotated tasks:

```yaml
      - name: Run unit tests
        run: task test

      - name: vet
        run: task vet
```

Commit `Taskfile.yaml`, the template, and the generated workflow. CI runs
`task2ci -check` (which the template includes) on every push, so any
drift fails the build.

## Annotation syntax

The same syntax works in two places:

- **In `Taskfile.yaml`**, on any task — opts that task into a tag:

  ```text
  # @ci: <tag>              # tag only
  # @ci: <tag> | <step name>  # tag plus display-name override
  ```

- **In a template**, under `.task2ci/workflows/<name>.{yaml,yml}` — marks a
  splice point. Templates do **not** accept the `| <step name>` form; step
  naming is owned by the task side.

### Step name resolution

The step's `name:` in the generated workflow is chosen in this order:

1. Override from the annotation (`| step name`) in `Taskfile.yaml`
2. The task's `desc:`
3. The task name itself

## Templates

- Live under `.task2ci/workflows/<name>.yaml` (or `.yml` — both accepted).
- Are plain GitHub Actions workflow YAML; everything except `# @ci: <tag>`
  placeholder comments is copied through verbatim.
- Render to `.github/workflows/<name>.yaml` (output is always `.yaml`).
- Get a single autogenerated-header comment prepended to the output.

Anything that should run in CI **other** than the task-driven steps — setup
actions, environment variables, conditionals, the drift-check step itself —
goes directly into the template in GHA's native syntax.

### Multiple workflows

Multiple template files produce multiple workflow files:

- `.task2ci/workflows/ci.yaml` → `.github/workflows/ci.yaml`
- `.task2ci/workflows/release.yaml` → `.github/workflows/release.yaml`

Each is independent.

### Warnings

- A `# @ci: <tag>` annotation in `Taskfile.yaml` with no matching template
  placeholder prints a warning (with a copy-pasteable snippet) and the
  task is left out of CI.
- A `# @ci: <tag>` placeholder in a template with no matching task prints a
  warning and the placeholder is removed from the generated output.

Both are warnings in plain generation but **fail `-check`** so CI catches
them before merge.

## CLI reference

```text
task2ci [flags]
```

- **_(no flags)_** — Render each template under `.task2ci/workflows/` to
  `.github/workflows/<name>.yaml`.
- **`-check`** — Compare what would be generated now against the on-disk
  workflow files. Exit non-zero on any drift or orphan tag/placeholder.
  Used in CI.
- **`-fix`** — Remove orphan `# @ci: <tag>` placeholders (tags no task
  uses) from templates in place. Doesn't regenerate workflows; run
  `task2ci` after.
- **`-init`** — Write a minimal starter template at
  `.task2ci/workflows/ci.yaml`. Refuses to overwrite.
- **`-license`** — Print the license (MIT) and exit.
- **`-taskfile <path>`** — Path to a Taskfile. May be repeated to scan
  multiple files. Default: auto-discover (`Taskfile.yml` →
  `taskfile.yml` → `Taskfile.yaml` → `taskfile.yaml` → `.dist` variants).

`-check`, `-fix`, and `-init` are mutually exclusive.

## How it adapts to your toolchain

The default behavior is language-agnostic: inserted steps run
`task <task-name>`, and the user's template installs `task` however it
wants (the [go-task/setup-task](https://github.com/go-task/setup-task)
action is the typical choice).

The one Go-specific optimization: if `task2ci` finds a `go.mod` in the
working directory that registers `github.com/go-task/task/v3/cmd/task` as
a `tool` directive (Go 1.24+ tool dependencies), the generated `run:` lines
use `go tool task <name>` instead. CI then uses the exact go-task version
pinned in your `go.mod` — no separate install step needed (though you
still need `actions/setup-go` in your template).

Non-Go projects: just include
`uses: go-task/setup-task@v2` in the template; the `run: task <name>`
lines work the same way.

## Working with AI tools

If you use AI coding assistants (Claude Code, Cursor, Copilot, etc.) in
your project, paste the snippet below into your `AGENTS.md` /
`CLAUDE.md` / `.cursorrules` / whatever project-rules file your tool
reads. It keeps them from trying to hand-edit the generated workflow
when they should be editing the template instead.

```text
This project uses task2ci to generate GitHub Actions workflows.

- Source of truth:
  - `.task2ci/workflows/<name>.yaml` — workflow templates.
  - `Taskfile.yaml` — task definitions, opted into CI via
    `# @ci: <tag>` annotations.
- Generated, do not hand-edit:
  - `.github/workflows/<name>.yaml` — regenerated from the templates
    on every `task2ci` run.
- After changing a template or annotation, run `task2ci` to regenerate
  the workflow files and commit the result.
- CI runs `task2ci -check` and fails on drift, orphan tag
  annotations, or orphan placeholders.
- Use `task2ci -fix` to delete orphan placeholders from templates.
- Use `task2ci -init` to scaffold a starter template.

Full docs: https://arnested.github.io/task2ci/
```

## Source

[github.com/arnested/task2ci](https://github.com/arnested/task2ci)

## License

MIT.

```text
MIT License
===========

Copyright (c) 2026 Arne Jørgensen

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
```
