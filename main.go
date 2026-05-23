package main

import (
	"bytes"
	_ "embed"
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
func runInit() {
	if _, err := os.Stat(initDefaultPath); err == nil {
		log.Fatalf("%s already exists; refusing to overwrite. Edit it directly or delete it first.", initDefaultPath)
	} else if !os.IsNotExist(err) {
		log.Fatalf("Could not stat %s: %v", initDefaultPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(initDefaultPath), 0o755); err != nil {
		log.Fatalf("Error creating %s: %v", filepath.Dir(initDefaultPath), err)
	}
	template, flavor := pickInitTemplate()
	if err := os.WriteFile(initDefaultPath, []byte(template), 0o644); err != nil {
		log.Fatalf("Error writing %s: %v", initDefaultPath, err)
	}
	fmt.Printf("✅ Wrote %s (%s starter). Annotate Taskfile tasks with `# @ci: test` (or rename the tag) to fill the slot, then run task2ci.\n", initDefaultPath, flavor)
}

// runFix mutates templates in place: removes `# @ci: <tag>` placeholder lines
// whose tag has no matching task. Returns the number of placeholders removed.
func runFix(templates []string, tasksByTag map[string][]Task) int {
	removed := 0
	for _, tpath := range templates {
		data, err := os.ReadFile(tpath)
		if err != nil {
			log.Fatalf("Error reading template %s: %v", tpath, err)
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
				log.Fatalf("Error writing %s: %v", tpath, err)
			}
			fmt.Printf("✏️  %s: removed orphan placeholder(s)\n", tpath)
		}
	}
	if removed == 0 {
		fmt.Println("✅ No orphan placeholders found.")
	} else {
		fmt.Printf("✅ Removed %d orphan placeholder(s). Run task2ci to regenerate workflows.\n", removed)
	}
	return removed
}

func main() {
	checkPtr := flag.Bool("check", false, "Fail if any generated workflow has drifted from its template")
	fixPtr := flag.Bool("fix", false, "Remove orphan `# @ci: <tag>` placeholders from templates (in place)")
	initPtr := flag.Bool("init", false, "Write a minimal starter template at "+initDefaultPath+" and exit")
	licensePtr := flag.Bool("license", false, "Print the license and exit")
	var taskfiles stringList
	flag.Var(&taskfiles, "taskfile", "Path to a Taskfile. May be repeated. Default: auto-discover Taskfile.yml/.yaml etc.")
	flag.Parse()

	if *licensePtr {
		_, _ = os.Stdout.Write(licenseText)
		return
	}

	exclusive := 0
	for _, b := range []bool{*checkPtr, *fixPtr, *initPtr} {
		if b {
			exclusive++
		}
	}
	if exclusive > 1 {
		log.Fatalf("-check, -fix, and -init are mutually exclusive")
	}

	if *initPtr {
		runInit()
		return
	}

	if len(taskfiles) == 0 {
		found := findTaskfile()
		if found == "" {
			log.Fatalf("No Taskfile found. Looked for (in order): %s. Use -taskfile to specify a path.",
				strings.Join(taskfileSearchOrder, ", "))
		}
		taskfiles = stringList{found}
	} else {
		for _, p := range taskfiles {
			if _, err := os.Stat(p); err != nil {
				log.Fatalf("Taskfile %q (from -taskfile): %v", p, err)
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
		for tag, tasks := range readTasks(path, taskCmd) {
			for _, t := range tasks {
				if prev, dup := seenTasks[t.Name]; dup {
					fmt.Fprintf(os.Stderr,
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
		log.Fatalf("Error listing templates under %s: %v", TemplateDir, err)
	}
	if len(templates) == 0 {
		log.Fatalf("No templates found under %s/. Create at least one workflow template (with `# @ci: <tag>` placeholders) before running task2ci, or run `task2ci -init` to scaffold one.", TemplateDir)
	}

	if *fixPtr {
		runFix(templates, tasksByTag)
		return
	}

	// Render each template to its target path, collecting which tags any
	// template referenced (used for orphan warnings below).
	rendered := make(map[string][]byte, len(templates))
	usedTags := make(map[string]bool)
	for _, tpath := range templates {
		data, err := os.ReadFile(tpath)
		if err != nil {
			log.Fatalf("Error reading template %s: %v", tpath, err)
		}
		out, refs := renderTemplate(tpath, data, tasksByTag)
		for _, r := range refs {
			usedTags[r] = true
		}
		rendered[outputPathFor(tpath)] = out
	}

	orphans := warnOrphans(os.Stderr, tasksByTag, usedTags)

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
				log.Fatalf("Check failed: could not read existing workflow %s: %v", p, err)
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
				msgs = append(msgs, fmt.Sprintf("drift in: %s", strings.Join(drifted, ", ")))
			}
			log.Fatalf("❌ ERROR: %s. Fix the issues and run task2ci locally before committing.", strings.Join(msgs, "; "))
		}
		fmt.Println("✅ Generated workflows are up to date.")
		return
	}

	if err := os.MkdirAll(OutputDir, 0o755); err != nil {
		log.Fatalf("Error creating %s: %v", OutputDir, err)
	}
	for _, p := range outPaths {
		if err := os.WriteFile(p, rendered[p], 0o644); err != nil {
			log.Fatalf("Error writing %s: %v", p, err)
		}
		fmt.Printf("✅ Wrote %s\n", p)
	}
}

// listTemplates returns paths to *.yaml and *.yml files under dir (sorted).
func listTemplates(dir string) ([]string, error) {
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
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
func renderTemplate(tpath string, data []byte, tasksByTag map[string][]Task) ([]byte, []string) {
	var refs []string
	out := placeholderRE.ReplaceAllFunc(data, func(match []byte) []byte {
		sub := placeholderRE.FindSubmatch(match)
		indent := string(sub[1])
		tag := string(sub[2])
		refs = append(refs, tag)
		tasks := tasksByTag[tag]
		if len(tasks) == 0 {
			return nil
		}
		return []byte(renderBlock(tasks, indent) + "\n")
	})

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "# THIS FILE IS AUTOGENERATED by task2ci from %s. DO NOT EDIT.\n", tpath)
	buf.Write(out)
	return stripTrailingWhitespace(buf.Bytes()), refs
}

// renderBlock renders a series of steps at the given indent, with a blank line
// between consecutive steps.
func renderBlock(tasks []Task, indent string) string {
	parts := make([]string, len(tasks))
	for i, t := range tasks {
		parts[i] = renderStep(t, indent)
	}
	return strings.Join(parts, "\n\n")
}

// renderStep encodes one step via yaml.v3 (so values are quoted correctly when
// needed) then re-indents and prefixes the first line with `- `.
func renderStep(t Task, indent string) string {
	type stepFields struct {
		Name string `yaml:"name,omitempty"`
		Run  string `yaml:"run,omitempty"`
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(stepFields{Name: t.Step, Run: t.Run}); err != nil {
		log.Fatalf("Encoding step for task %q: %v", t.Name, err)
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
	return strings.Join(lines, "\n")
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
func readTasks(path, taskCmd string) map[string][]Task {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Error reading %s: %v", path, err)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		log.Fatalf("Error parsing %s: %v", path, err)
	}
	tasksNode := findTasksNode(&root)
	if tasksNode == nil {
		log.Fatalf("Could not find 'tasks' block in %s", path)
	}

	out := make(map[string][]Task)
	for i := 0; i+1 < len(tasksNode.Content); i += 2 {
		keyNode := tasksNode.Content[i]
		valueNode := tasksNode.Content[i+1]
		taskName := keyNode.Value

		comments := []string{keyNode.HeadComment, keyNode.LineComment}
		if valueNode.Kind == yaml.MappingNode {
			for _, c := range valueNode.Content {
				comments = append(comments, c.HeadComment, c.LineComment)
			}
		}
		block := strings.Join(comments, "\n")
		if !strings.Contains(block, "@ci:") {
			continue
		}
		// First @ci: occurrence on its own line is the annotation we care about.
		parts := strings.SplitN(block, "@ci:", 2)
		tagLine := strings.SplitN(parts[1], "\n", 2)[0]
		tag, stepName := parseAnnotation(tagLine)
		if tag == "" {
			continue
		}

		name := stepName
		if name == "" {
			name = taskDesc(valueNode)
		}
		if name == "" {
			name = taskName
		}
		out[tag] = append(out[tag], Task{
			Name: taskName,
			Step: name,
			Run:  fmt.Sprintf("%s %s", taskCmd, taskName),
		})
	}
	return out
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
	for _, raw := range strings.Split(content, "\n") {
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
		if strings.HasPrefix(line, "tool ") {
			if strings.TrimSpace(strings.TrimPrefix(line, "tool ")) == path {
				return true
			}
		}
	}
	return false
}
