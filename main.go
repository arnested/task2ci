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
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed LICENSE.md
var licenseText []byte

const (
	OutputDir      = ".github/workflows"
	TemplateDir    = ".task2ci/workflows"
	ModulePath     = "arnested.dk/go/task2ci"
	GoTaskToolPath = "github.com/go-task/task/v3/cmd/task"
)

// taskfileSearchOrder mirrors go-task's resolution: case variants of .yml
// before .yaml, dist variants last.
var taskfileSearchOrder = []string{
	"Taskfile.yml",
	"taskfile.yml",
	"Taskfile.yaml",
	"taskfile.yaml",
	"Taskfile.dist.yml",
	"taskfile.dist.yml",
	"Taskfile.dist.yaml",
	"taskfile.dist.yaml",
}

// findTaskfile returns the path of the first existing Taskfile in the search
// order, or "" if none of the candidates exist.
func findTaskfile() string {
	for _, name := range taskfileSearchOrder {
		if info, err := os.Stat(name); err == nil && !info.IsDir() {
			return name
		}
	}
	return ""
}

// placeholderRE matches `# @ci: <tag>` lines (any leading whitespace, anything
// after the tag is tolerated). The line including its trailing newline is the
// match, so empty substitutions cleanly remove the line.
var placeholderRE = regexp.MustCompile(`(?m)^([ \t]*)# @ci:[ \t]*(\S+).*$\n?`)

// stringList is a flag.Value that collects repeated occurrences of a string
// flag (e.g. `-taskfile a.yaml -taskfile b.yaml`).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// Task is one Taskfile task slotted into CI via an @ci annotation.
type Task struct {
	Name string // task identifier (key in Taskfile tasks map)
	Step string // resolved display name: annotation override > desc > task name
	Run  string // run command, e.g. "task test" or "go tool task test"
}

// initTemplateGo is the starter used when go.mod is present. Wires up
// actions/setup-go and uses `go tool task2ci -check` (assuming the user
// will register task2ci as a Go tool dependency).
const initTemplateGo = `---
name: ci
on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Check generated CI is up to date
        run: go tool task2ci -check
      # @ci: test
`

// initTemplateGeneric is the starter for non-Go projects. Installs go-task
// via the marketplace action and runs task2ci via `go run <module>@latest`
// (ubuntu-24.04 has Go preinstalled, and we only need task2ci for a single
// `-check` step). Setup steps for the project's own toolchain are commented
// out as examples for the user to swap in.
const initTemplateGeneric = `---
name: ci
on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4

      # Set up the toolchain your tasks need. Common examples:
      #   - uses: actions/setup-node@v4
      #     with: { node-version-file: .nvmrc }
      #   - uses: actions/setup-python@v5
      #     with: { python-version-file: .python-version }
      #   - uses: ruby/setup-ruby@v1
      #     with: { bundler-cache: true }

      - uses: go-task/setup-task@v2

      - name: Check generated CI is up to date
        # ubuntu-24.04 has Go preinstalled, so go run works without an
        # explicit actions/setup-go step. Swap to a release-binary download
        # or different runner if you'd rather not depend on that.
        run: go run arnested.dk/go/task2ci@latest -check
      # @ci: test
`

// initDefaultPath is the path the -init flag writes to. Fixed for now;
// matches the convention documented in README and AGENTS.
const initDefaultPath = ".task2ci/workflows/ci.yaml"

// pickInitTemplate returns the Go-flavored starter when go.mod exists, and
// the generic starter otherwise. The detection is intentionally just a
// presence check — we don't peek inside go.mod.
func pickInitTemplate() (template, flavor string) {
	if _, err := os.Stat("go.mod"); err == nil {
		return initTemplateGo, "Go"
	}
	return initTemplateGeneric, "generic"
}

// runInit writes a minimal starter template. Refuses to overwrite.
func runInit() error {
	if _, err := os.Stat(initDefaultPath); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite. Edit it directly or delete it first", initDefaultPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("could not stat %s: %w", initDefaultPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(initDefaultPath), 0o750); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(initDefaultPath), err)
	}
	template, flavor := pickInitTemplate()
	if err := os.WriteFile(initDefaultPath, []byte(template), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", initDefaultPath, err)
	}
	fmt.Printf("✅ Wrote %s (%s starter). Annotate Taskfile tasks with `# @ci: test` (or rename the tag) to fill the slot, then run task2ci.\n", initDefaultPath, flavor)
	return nil
}

// runFix mutates templates in place: removes `# @ci: <tag>` placeholder lines
// whose tag has no matching task. Returns the number of placeholders removed.
func runFix(templates []string, tasksByTag map[string][]Task) (int, error) {
	removed := 0
	for _, tpath := range templates {
		data, err := os.ReadFile(tpath)
		if err != nil {
			return removed, fmt.Errorf("reading template %s: %w", tpath, err)
		}
		newData := placeholderRE.ReplaceAllFunc(data, func(match []byte) []byte {
			sub := placeholderRE.FindSubmatch(match)
			if _, ok := tasksByTag[string(sub[2])]; !ok {
				removed++
				return nil
			}
			return match
		})
		if !bytes.Equal(data, newData) {
			if err := os.WriteFile(tpath, newData, 0o644); err != nil {
				return removed, fmt.Errorf("writing %s: %w", tpath, err)
			}
			fmt.Printf("✏️  %s: removed orphan placeholder(s)\n", tpath)
		}
	}
	if removed == 0 {
		fmt.Println("✅ No orphan placeholders found.")
	} else {
		fmt.Printf("✅ Removed %d orphan placeholder(s). Run task2ci to regenerate workflows.\n", removed)
	}
	return removed, nil
}

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

// listTemplates returns paths to *.yaml and *.yml files under dir (sorted).
func listTemplates(dir string) ([]string, error) {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yaml") || strings.HasSuffix(name, ".yml") {
			out = append(out, filepath.Join(dir, name))
		}
	}
	sort.Strings(out)
	return out, nil
}

// outputPathFor maps `.task2ci/workflows/<base>.{yaml,yml}` to
// `.github/workflows/<base>.yaml`. Output is always .yaml.
func outputPathFor(tpath string) string {
	base := filepath.Base(tpath)
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".yaml"), ".yml")
	return filepath.Join(OutputDir, base+".yaml")
}

// renderTemplate substitutes every `# @ci: <tag>` placeholder in data with the
// rendered step block for that tag. The autogenerated header is prepended.
// Returns the rendered bytes and the list of tags referenced (in source order,
// with duplicates).
func renderTemplate(tpath string, data []byte, tasksByTag map[string][]Task) ([]byte, []string, error) {
	var refs []string
	var renderErr error
	out := placeholderRE.ReplaceAllFunc(data, func(match []byte) []byte {
		if renderErr != nil {
			return match
		}
		sub := placeholderRE.FindSubmatch(match)
		indent := string(sub[1])
		tag := string(sub[2])
		refs = append(refs, tag)
		tasks := tasksByTag[tag]
		if len(tasks) == 0 {
			return nil
		}
		block, err := renderBlock(tasks, indent)
		if err != nil {
			renderErr = err
			return match
		}
		return []byte(block + "\n")
	})
	if renderErr != nil {
		return nil, refs, renderErr
	}

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# THIS FILE IS AUTOGENERATED by task2ci. DO NOT EDIT.\n")
	fmt.Fprintf(&buf, "# Source: %s\n", tpath)
	fmt.Fprintf(&buf, "# To change CI: edit the source template (or Taskfile.yaml), then run `task2ci`.\n")
	buf.Write(out)
	return stripTrailingWhitespace(buf.Bytes()), refs, nil
}

// renderBlock renders a series of steps at the given indent, with a blank line
// between consecutive steps.
func renderBlock(tasks []Task, indent string) (string, error) {
	parts := make([]string, len(tasks))
	for i, t := range tasks {
		s, err := renderStep(t, indent)
		if err != nil {
			return "", err
		}
		parts[i] = s
	}
	return strings.Join(parts, "\n\n"), nil
}

// renderStep encodes one step via yaml.v3 (so values are quoted correctly when
// needed) then re-indents and prefixes the first line with a "- ".
func renderStep(t Task, indent string) (string, error) {
	type stepFields struct {
		Name string `yaml:"name,omitempty"`
		Run  string `yaml:"run,omitempty"`
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(stepFields{Name: t.Step, Run: t.Run}); err != nil {
		return "", fmt.Errorf("encoding step for task %q: %w", t.Name, err)
	}
	_ = enc.Close()

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	for i, line := range lines {
		if i == 0 {
			lines[i] = indent + "- " + line
		} else {
			lines[i] = indent + "  " + line
		}
	}
	return strings.Join(lines, "\n"), nil
}

// stripTrailingWhitespace removes trailing spaces/tabs from each line.
// Belt-and-suspenders for any tool that ever flags trailing whitespace.
func stripTrailingWhitespace(b []byte) []byte {
	lines := bytes.Split(b, []byte("\n"))
	for i, line := range lines {
		lines[i] = bytes.TrimRight(line, " \t")
	}
	return bytes.Join(lines, []byte("\n"))
}

// warnOrphans reports tags used by tasks but not in any template, and
// placeholders in templates without matching tasks. Returns the total
// orphan count, so callers (like -check) can treat them as failures.
func warnOrphans(out io.Writer, tasksByTag map[string][]Task, usedTags map[string]bool) int {
	orphans := 0

	// Tag annotated in Taskfile but no template uses it.
	taskTags := make([]string, 0, len(tasksByTag))
	for tag := range tasksByTag {
		taskTags = append(taskTags, tag)
	}
	sort.Strings(taskTags)
	for _, tag := range taskTags {
		if !usedTags[tag] {
			names := make([]string, len(tasksByTag[tag]))
			for i, t := range tasksByTag[tag] {
				names[i] = t.Name
			}
			_, _ = fmt.Fprintf(out,
				"⚠️  Tag %q is used by task(s) %s but no template under %s references it.\n"+
					"    Add this placeholder line to an existing template (or create a new one):\n"+
					"\n"+
					"      # @ci: %s\n"+
					"\n"+
					"    Until then, these tasks will not appear in any workflow.\n",
				tag, strings.Join(names, ", "), TemplateDir, tag)
			orphans++
		}
	}

	// Placeholder in template but no task has the tag.
	templateTags := make([]string, 0, len(usedTags))
	for tag := range usedTags {
		templateTags = append(templateTags, tag)
	}
	sort.Strings(templateTags)
	for _, tag := range templateTags {
		if _, ok := tasksByTag[tag]; !ok {
			_, _ = fmt.Fprintf(out,
				"⚠️  Template placeholder `# @ci: %s` has no matching tasks; the placeholder will be removed in the generated workflow.\n",
				tag)
			orphans++
		}
	}

	return orphans
}

// readTasks reads the Taskfile, finds `@ci:` annotations, and groups tasks by
// tag. Order within a tag matches Taskfile order.
func readTasks(path, taskCmd string) (map[string][]Task, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	tasksNode := findTasksNode(&root)
	if tasksNode == nil {
		return nil, fmt.Errorf("could not find 'tasks' block in %s", path)
	}

	out := make(map[string][]Task)
	for i := 0; i+1 < len(tasksNode.Content); i += 2 {
		keyNode := tasksNode.Content[i]
		valueNode := tasksNode.Content[i+1]
		tag, name := parseTaskAnnotation(keyNode, valueNode)
		if tag == "" {
			continue
		}
		taskName := keyNode.Value
		if name == "" {
			name = taskName
		}
		out[tag] = append(out[tag], Task{
			Name: taskName,
			Step: name,
			Run:  fmt.Sprintf("%s %s", taskCmd, taskName),
		})
	}
	return out, nil
}

// parseTaskAnnotation walks every head and line comment on a task's key and
// value nodes, finds the first "@ci: <tag>" annotation, and resolves the
// display name (annotation override → task desc → ""). Returns ("", "") when
// no annotation is present or the annotation is empty.
func parseTaskAnnotation(keyNode, valueNode *yaml.Node) (tag, name string) {
	comments := []string{keyNode.HeadComment, keyNode.LineComment}
	if valueNode.Kind == yaml.MappingNode {
		for _, c := range valueNode.Content {
			comments = append(comments, c.HeadComment, c.LineComment)
		}
	}
	block := strings.Join(comments, "\n")
	if !strings.Contains(block, "@ci:") {
		return "", ""
	}
	parts := strings.SplitN(block, "@ci:", 2)
	tagLine := strings.SplitN(parts[1], "\n", 2)[0]
	tag, name = parseAnnotation(tagLine)
	if tag == "" {
		return "", ""
	}
	if name == "" {
		name = taskDesc(valueNode)
	}
	return tag, name
}

// parseAnnotation splits an "@ci:" comment payload into (tag, optional step
// name). Syntax: "tag" or "tag | step name". Whitespace around either side is
// trimmed. Returns ("", "") for an empty line.
func parseAnnotation(line string) (tag, stepName string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	before, after, hasPipe := strings.Cut(line, "|")
	if !hasPipe {
		return strings.TrimSpace(before), ""
	}
	return strings.TrimSpace(before), strings.TrimSpace(after)
}

// taskDesc returns the `desc` string for a task value node, or "" if absent.
func taskDesc(valueNode *yaml.Node) string {
	if valueNode == nil || valueNode.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(valueNode.Content); i += 2 {
		if valueNode.Content[i].Value == "desc" {
			return valueNode.Content[i+1].Value
		}
	}
	return ""
}

// findTasksNode traverses the AST to find the "tasks" map.
func findTasksNode(root *yaml.Node) *yaml.Node {
	if root.Kind == yaml.DocumentNode {
		root = root.Content[0]
	}
	if root.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(root.Content); i += 2 {
			if root.Content[i].Value == "tasks" {
				return root.Content[i+1]
			}
		}
	}
	return nil
}

// isToolDependency reports whether the local go.mod registers the given module
// path as a Go tool dependency. Used to decide whether to invoke a tool via
// `<name>` or `go tool <name>`.
func isToolDependency(path string) bool {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		return false
	}
	return hasToolDirective(string(data), path)
}

// hasToolDirective returns true if the go.mod content has a `tool <path>`
// directive, either single-line or inside a `tool ( ... )` block.
func hasToolDirective(content, path string) bool {
	inBlock := false
	for raw := range strings.SplitSeq(content, "\n") {
		line := strings.TrimSpace(raw)
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}
		if inBlock {
			if line == ")" {
				inBlock = false
				continue
			}
			if line == path {
				return true
			}
			continue
		}
		if line == "tool (" {
			inBlock = true
			continue
		}
		if rest, ok := strings.CutPrefix(line, "tool "); ok {
			if strings.TrimSpace(rest) == path {
				return true
			}
		}
	}
	return false
}
