// Package main implements task2ci, a CLI that generates GitHub Actions
// workflows from Taskfile annotations and workflow templates. See README.md
// or `task2ci --help`.
package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
)

//go:embed LICENSE.md
var licenseText []byte

const (
	OutputDir      = ".github/workflows"
	TemplateDir    = ".task2ci/workflows"
	ModulePath     = "arnested.dk/go/task2ci"
	GoTaskToolPath = "github.com/go-task/task/v3/cmd/task"
)

// stringList is a flag.Value that collects repeated occurrences of a string
// flag (e.g. `-taskfile a.yaml -taskfile b.yaml`).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		log.Fatal(err)
	}
}

// run is the testable entry point. It parses args via a fresh FlagSet (so
// concurrent or repeated calls don't share state), and returns a non-nil
// error on any failure path that main() would have called log.Fatal on.
//
// stdout receives success / info output. stderr receives warnings.
//
//nolint:gocyclo,cyclop,funlen // Top-level orchestration: linear sequence of
// mode dispatch and I/O steps. Splitting would just hide the flow without
// simplifying it.
func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("task2ci", flag.ContinueOnError)
	fs.SetOutput(stderr)
	checkPtr := fs.Bool("check", false, "Fail if any generated workflow has drifted from its template")
	fixPtr := fs.Bool("fix", false, "Remove orphan '# @ci: <tag>' placeholders from templates (in place)")
	initPtr := fs.Bool("init", false, "Write a minimal starter template at "+initDefaultPath+" and exit")
	licensePtr := fs.Bool("license", false, "Print the license and exit")
	var taskfiles stringList
	fs.Var(&taskfiles, "taskfile", "Path to a Taskfile. May be repeated. Default: auto-discover Taskfile.yml/.yaml etc.")
	fs.Usage = func() {
		out := fs.Output()
		_, _ = fmt.Fprintln(out, `task2ci generates GitHub Actions workflows from your Taskfile.

Usage:
  task2ci [flags]

Flags:`)
		fs.PrintDefaults()
		_, _ = fmt.Fprintln(out, `
Layout:
  .task2ci/workflows/<name>.yaml  source-of-truth workflow templates
  Taskfile.yaml                   task definitions (annotated '# @ci: <tag>')
  .github/workflows/<name>.yaml   generated — DO NOT hand-edit

For AI tools / contributors: edit the templates under .task2ci/workflows/
and the Taskfile, not the generated files. Run 'task2ci' to regenerate,
'task2ci -check' to verify on-disk matches.

Docs: https://arnested.github.io/task2ci/`)
	}
	if err := fs.Parse(args); err != nil {
		// -h / -help triggers the Usage function above; suppress the
		// "flag: help requested" pseudo-error that flag.Parse returns.
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return fmt.Errorf("parsing flags: %w", err)
	}

	if *licensePtr {
		_, _ = stdout.Write(licenseText)
		return nil
	}

	exclusive := 0
	for _, b := range []bool{*checkPtr, *fixPtr, *initPtr} {
		if b {
			exclusive++
		}
	}
	if exclusive > 1 {
		return errors.New("-check, -fix, and -init are mutually exclusive")
	}

	if *initPtr {
		return runInit()
	}

	if len(taskfiles) == 0 {
		found := findTaskfile()
		if found == "" {
			return fmt.Errorf("no Taskfile found. Looked for (in order): %s. Use -taskfile to specify a path",
				strings.Join(taskfileSearchOrder, ", "))
		}
		taskfiles = stringList{found}
	} else {
		for _, p := range taskfiles {
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("taskfile %q (from -taskfile): %w", p, err)
			}
		}
	}

	taskCmd := "task"
	if isToolDependency(GoTaskToolPath) {
		taskCmd = "go tool task"
	}

	tasksByTag := make(map[string][]Task)
	seenTasks := make(map[string]string) // task name → file it was first seen in
	for _, path := range taskfiles {
		tasks, err := readTasks(path, taskCmd)
		if err != nil {
			return err
		}
		for tag, tList := range tasks {
			for _, t := range tList {
				if prev, dup := seenTasks[t.Name]; dup {
					_, _ = fmt.Fprintf(stderr,
						"⚠️  Task %q appears in %s and %s; the generated `task %s` step is ambiguous unless go-task can dispatch it deterministically (e.g. via aliases or includes).\n",
						t.Name, prev, path, t.Name)
				} else {
					seenTasks[t.Name] = path
				}
				tasksByTag[tag] = append(tasksByTag[tag], t)
			}
		}
	}

	templates, err := listTemplates(TemplateDir)
	if err != nil {
		return fmt.Errorf("listing templates under %s: %w", TemplateDir, err)
	}
	if len(templates) == 0 {
		return fmt.Errorf("no templates found under %s/. Create at least one workflow template (with `# @ci: <tag>` placeholders) before running task2ci, or run `task2ci -init` to scaffold one", TemplateDir)
	}

	if *fixPtr {
		_, err := runFix(templates, tasksByTag)
		return err
	}

	// Render each template to its target path, collecting which tags any
	// template referenced (used for orphan warnings below).
	rendered := make(map[string][]byte, len(templates))
	usedTags := make(map[string]bool)
	for _, tpath := range templates {
		data, err := os.ReadFile(tpath)
		if err != nil {
			return fmt.Errorf("reading template %s: %w", tpath, err)
		}
		out, refs, err := renderTemplate(tpath, data, tasksByTag)
		if err != nil {
			return err
		}
		for _, r := range refs {
			usedTags[r] = true
		}
		rendered[outputPathFor(tpath)] = out
	}

	orphans := warnOrphans(stderr, tasksByTag, usedTags)

	outPaths := make([]string, 0, len(rendered))
	for p := range rendered {
		outPaths = append(outPaths, p)
	}
	sort.Strings(outPaths)

	if *checkPtr {
		var drifted []string
		for _, p := range outPaths {
			existing, err := os.ReadFile(p)
			if err != nil {
				return fmt.Errorf("check failed: could not read existing workflow %s: %w", p, err)
			}
			if !bytes.Equal(existing, rendered[p]) {
				drifted = append(drifted, p)
			}
		}
		if orphans > 0 || len(drifted) > 0 {
			var msgs []string
			if orphans > 0 {
				msgs = append(msgs, fmt.Sprintf("%d orphan tag/placeholder warning(s) above", orphans))
			}
			if len(drifted) > 0 {
				msgs = append(msgs, "drift in: "+strings.Join(drifted, ", "))
			}
			return fmt.Errorf("❌ ERROR: %s. Fix the issues and run task2ci locally before committing", strings.Join(msgs, "; "))
		}
		_, _ = fmt.Fprintln(stdout, "✅ Generated workflows are up to date.")
		return nil
	}

	if err := os.MkdirAll(OutputDir, 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", OutputDir, err)
	}
	for _, p := range outPaths {
		if err := os.WriteFile(p, rendered[p], 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", p, err)
		}
		_, _ = fmt.Fprintf(stdout, "✅ Wrote %s\n", p)
	}
	return nil
}
