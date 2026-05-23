# task2ci

Generate GitHub Actions workflows from your `Taskfile.yml`, so the commands you
run locally are the same ones CI runs — guaranteed, not by hand.

## What it does

Pick the [go-task] tasks you want CI to run and mark them with a `# @ci: <tag>`
comment. Run `task2ci`, and it writes a workflow under `.github/workflows/`
whose jobs invoke those tasks via `task <name>`. A separate `task2ci -check`
mode fails CI if the generated workflow drifts from `Taskfile.yml`, so the two
can't quietly diverge.

[go-task]: https://taskfile.dev/

## Why

A typical Go project ends up with two parallel lists of commands:

- `Taskfile.yml` — what developers run locally (`task test`, `task lint`)
- `.github/workflows/ci.yml` — what CI runs (`go test ./...`, `golangci-lint run`)

These two drift. Someone adds a new check to the Taskfile but forgets the
workflow, or vice versa, and "works on my machine" creeps in. The fix is to
keep one source of truth and derive the other.

`task2ci` makes the `Taskfile.yml` the source of truth. CI just invokes the
same `task <name>` you'd run locally, on the same OS image, with the same
commands. To opt a task into CI, you add a comment; to keep it local-only
(like `tidy` or `install-hooks`), you don't.

## Quick start

In a project that already has a `Taskfile.yml`:

```sh
# 1. Install task2ci as a Go tool dependency (Go 1.24+)
go get -tool arnested.dk/go/task2ci

# 2. Scaffold a config
go tool task2ci -init
```

That writes a starter `.task2ci.yml` with the action defaults filled in and a
commented-out tag example. Define one or more tags:

```yaml
# .task2ci.yml
---
actions:
  checkout: actions/checkout@v4
  setup-task: go-task/setup-task@v2
  setup-task2ci: arnested/setup-task2ci@v1

tags:
  test:
    workflow: ci
    runs-on: ubuntu-latest
  build:
    workflow: ci
    runs-on: ubuntu-latest
```

Annotate the tasks you want in CI:

```yaml
# Taskfile.yml
---
version: '3'

tasks:
  # @ci: build
  build:
    desc: Build the binary
    cmd: go build .

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

Generate the workflow:

```sh
go tool task2ci
```

You now have `.github/workflows/ci.yml`, a job `build` that runs `task build`,
and a job `test` that runs `task test` and `task vet`. Commit both
`Taskfile.yml` and the generated workflow.

In CI, the very first step in every job is `task2ci -check`, which fails the
build if the workflow no longer matches the Taskfile — so reviewers see the
mismatch instead of a stale CI run masking it.

## Annotation syntax

```text
# @ci: <tag>
# @ci: <tag> | <step name>
```

- `<tag>` is a key defined under `tags:` in `.task2ci.yml`.
- The optional step name (after a `|`) overrides the step's display name in
  the workflow. The actual command run is always `task <task-name>`, so this
  only affects what shows up in the GitHub Actions UI.

The annotation can be placed as a head comment on the task key or as a line
comment on any of its inner fields. Tasks without an annotation are left
out of CI.

### Step name resolution

The step's `name:` in the generated workflow is chosen in this order:

1. Override from the annotation (`| step name`)
2. The task's `desc:`
3. The task name itself

So `desc:` doubles as a default display name — concise descriptions show up
neatly in CI logs.

## Configuration reference (`.task2ci.yml`)

```yaml
actions:
  checkout: actions/checkout@v4          # default
  setup-task: go-task/setup-task@v2      # default
  setup-task2ci: arnested/setup-task2ci@v1  # default

tags:
  <tag-name>:
    workflow: <workflow-name>   # default: "taskfile"
    job: <job-name>             # default: the tag name
    runs-on: <runner-label>     # required
```

### `actions`

Overrides for the action references injected into every job. All three fields
have defaults — set them to pin a different version, swap in a fork, or use a
locally vendored action.

| Field | Used for | Default |
|-------|----------|---------|
| `checkout` | First step in every job | `actions/checkout@v4` |
| `setup-task` | Installs `go-task` (skipped if it's a Go tool dep) | `go-task/setup-task@v2` |
| `setup-task2ci` | Installs `task2ci` (skipped if it's a Go tool dep) | `arnested/setup-task2ci@v1` |

### `tags`

Each tag describes a CI destination. Multiple tags can target the same job
(steps then group together), but they must share the same `runs-on` — a
conflict fails generation with a clear error.

| Field | Meaning | Default |
|-------|---------|---------|
| `workflow` | Output filename (without extension) under `.github/workflows/` | `taskfile` |
| `job` | Job key inside the workflow | The tag name |
| `runs-on` | GitHub Actions runner label | _required_ |

Tag names are referenced verbatim from `@ci:` annotations. Any annotation
pointing at an undefined tag triggers a stderr warning and that task is
skipped; the rest still generate.

### Editor support

The repository ships `task2ci.schema.json`, a JSON Schema (draft 2020-12) that
yaml-language-server (and similar) will use for completion and validation if
you reference it at the top of your config:

```yaml
# yaml-language-server: $schema=https://arnested.dk/schemas/task2ci.schema.json
---
actions:
  ...
```

The same schema is embedded in the binary and used by `-check` to validate
your config — see below.

## CLI reference

```text
task2ci [flags]
```

| Flag | Behavior |
|------|----------|
| _(none)_ | Generate workflow files under `.github/workflows/`, one per distinct `workflow:` in your tag definitions. |
| `-check` | Validate `.task2ci.yml` against the embedded schema, then fail if any generated workflow on disk differs from what would be produced now. Used in CI. |
| `-init` | Write a starter `.task2ci.yml`. Refuses to overwrite an existing file. |

## How it adapts to your toolchain

`task2ci` inspects your `go.mod` and picks the most direct path it can.

### `go-task` as a tool dependency

If `github.com/go-task/task/v3/cmd/task` appears as a Go `tool` directive in
`go.mod`, generated workflows skip the `go-task/setup-task` step and invoke
tasks via `go tool task <name>` instead of `task <name>`. This relies on Go
1.24+ tool dependencies and means CI uses the exact go-task version pinned in
your project.

### `task2ci` as a tool dependency

If `arnested.dk/go/task2ci` appears as a Go `tool` directive, the workflow's
drift-check step runs `go tool task2ci -check` instead of installing the
binary via the `arnested/setup-task2ci` action. Same rationale: version
pinning by `go.mod`.

If neither is a tool dep, the workflow falls back to the marketplace actions.

## Multiple workflows

Define tags with different `workflow:` values to produce separate workflow
files:

```yaml
tags:
  ci-test:
    workflow: ci
    job: test
    runs-on: ubuntu-latest
  release-build:
    workflow: release
    job: build
    runs-on: ubuntu-latest
```

Produces both `.github/workflows/ci.yml` and `.github/workflows/release.yml`.
Each file is independent and can have its own job structure.

## CI integration

Once `.github/workflows/<name>.yml` is committed, push to GitHub — Actions
picks it up automatically. The first task-running step in every job is
`task2ci -check`, so any change to `Taskfile.yml` that isn't reflected in the
committed workflow will fail CI immediately with a clear "drift detected"
message.

Locally, run `task2ci` to regenerate and commit the diff. A pre-commit hook
that runs `go tool task2ci -check` is a good way to catch this before
pushing.

## Status

Personal tool, used in production by the author. APIs (config schema, CLI
flags, generated workflow structure) may change between minor versions until
1.0.

## License

See [LICENSE.md](LICENSE.md).
