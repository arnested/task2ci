package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---------- pure helpers ----------

func TestHasToolDirective(t *testing.T) {
	cases := []struct {
		name, content, path string
		want                bool
	}{
		{"empty", "", "x.com/foo", false},
		{"single-line match", "tool x.com/foo\n", "x.com/foo", true},
		{"single-line different path", "tool x.com/bar\n", "x.com/foo", false},
		{"block match", "tool (\n\tx.com/foo\n)\n", "x.com/foo", true},
		{"block second entry", "tool (\n\tx.com/foo\n\tx.com/bar\n)\n", "x.com/bar", true},
		{"block no match", "tool (\n\tx.com/foo\n)\n", "x.com/baz", false},
		{"block with line comment", "tool (\n\tx.com/foo // why\n)\n", "x.com/foo", true},
		{"single-line trailing comment", "tool x.com/foo // pinned\n", "x.com/foo", true},
		{"prefix is not match", "tool x.com/foobar\n", "x.com/foo", false},
		{"require directive not matched", "require x.com/foo v1.2.3\n", "x.com/foo", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := hasToolDirective(c.content, c.path); got != c.want {
				t.Errorf("hasToolDirective(%q, %q) = %v, want %v", c.content, c.path, got, c.want)
			}
		})
	}
}

func TestParseAnnotation(t *testing.T) {
	cases := []struct {
		input, wantTag, wantStep string
	}{
		{"build", "build", ""},
		{"  build  ", "build", ""},
		{"build | Run the build", "build", "Run the build"},
		{"build|Run the build", "build", "Run the build"},
		{"build   |   Run all tests", "build", "Run all tests"},
		{"build | name: with colon, comma & quote \"x\"", "build", "name: with colon, comma & quote \"x\""},
		{"build |", "build", ""},
		{"", "", ""},
		{"   ", "", ""},
		{"multi word tag without pipe", "multi word tag without pipe", ""},
	}
	for _, c := range cases {
		t.Run(c.input, func(t *testing.T) {
			gotTag, gotStep := parseAnnotation(c.input)
			if gotTag != c.wantTag || gotStep != c.wantStep {
				t.Errorf("parseAnnotation(%q) = (%q, %q), want (%q, %q)",
					c.input, gotTag, gotStep, c.wantTag, c.wantStep)
			}
		})
	}
}

func TestTaskDesc(t *testing.T) {
	cases := []struct {
		name, yamlSrc, want string
	}{
		{"present", "desc: build the thing\ncmd: go build\n", "build the thing"},
		{"absent", "cmd: go test\n", ""},
		{"present with other fields", "cmd: go test\ndesc: run tests\nvars:\n  X: 1\n", "run tests"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(c.yamlSrc), &doc); err != nil {
				t.Fatal(err)
			}
			if got := taskDesc(doc.Content[0]); got != c.want {
				t.Errorf("taskDesc = %q, want %q", got, c.want)
			}
		})
	}
}

func TestTaskDescNilOrWrongKind(t *testing.T) {
	if got := taskDesc(nil); got != "" {
		t.Errorf("taskDesc(nil) = %q, want empty", got)
	}
	scalar := &yaml.Node{Kind: yaml.ScalarNode, Value: "x"}
	if got := taskDesc(scalar); got != "" {
		t.Errorf("taskDesc(scalar) = %q, want empty", got)
	}
}

func TestFindTasksNode(t *testing.T) {
	src := "version: '3'\ntasks:\n  build:\n    cmd: go build\n  test:\n    cmd: go test\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	node := findTasksNode(&doc)
	if node == nil || node.Kind != yaml.MappingNode {
		t.Fatalf("expected mapping node, got %+v", node)
	}
	if len(node.Content) != 4 {
		t.Errorf("expected 4 children, got %d", len(node.Content))
	}
}

func TestFindTasksNodeAbsent(t *testing.T) {
	src := "version: '3'\nincludes:\n  other: ./Other.yaml\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	if node := findTasksNode(&doc); node != nil {
		t.Errorf("expected nil when no tasks block, got %+v", node)
	}
}

// ---------- template rendering ----------

func TestOutputPathFor(t *testing.T) {
	cases := []struct{ in, want string }{
		{".task2ci/workflows/ci.yaml", filepath.Join(".github", "workflows", "ci.yaml")},
		{".task2ci/workflows/release.yml", filepath.Join(".github", "workflows", "release.yaml")},
	}
	for _, c := range cases {
		if got := outputPathFor(c.in); got != c.want {
			t.Errorf("outputPathFor(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRenderStepIndentAndDashAlignment(t *testing.T) {
	got := renderStep(Task{Name: "test", Step: "Run tests", Run: "task test"}, "      ")
	want := "      - name: Run tests\n        run: task test"
	if got != want {
		t.Errorf("renderStep mismatch:\nwant:\n%s\ngot:\n%s", want, got)
	}
}

func TestRenderStepQuotesValuesWithSpecialChars(t *testing.T) {
	// A step name containing a colon must round-trip through valid YAML.
	got := renderStep(Task{Step: "Run: integration tests", Run: "task it"}, "")
	if !strings.Contains(got, "name: 'Run: integration tests'") &&
		!strings.Contains(got, `name: "Run: integration tests"`) {
		t.Errorf("expected quoted name when value contains ':', got:\n%s", got)
	}
}

func TestRenderBlockBlankLineBetweenSteps(t *testing.T) {
	got := renderBlock([]Task{
		{Step: "first", Run: "task first"},
		{Step: "second", Run: "task second"},
	}, "  ")
	want := "  - name: first\n    run: task first\n\n  - name: second\n    run: task second"
	if got != want {
		t.Errorf("renderBlock mismatch:\nwant:\n%q\ngot:\n%q", want, got)
	}
}

func TestRenderTemplateSubstitutesPlaceholder(t *testing.T) {
	template := []byte("---\njobs:\n  test:\n    steps:\n      - uses: actions/checkout@v4\n      # @ci: test\n")
	tasks := map[string][]Task{
		"test": {{Name: "test", Step: "Run tests", Run: "task test"}},
	}
	out, refs := renderTemplate(".task2ci/workflows/ci.yaml", template, tasks)
	s := string(out)
	if !strings.Contains(s, "      - name: Run tests") {
		t.Errorf("expected substituted step at correct indent, got:\n%s", s)
	}
	if strings.Contains(s, "# @ci: test") {
		t.Errorf("placeholder should be replaced, but is still in output:\n%s", s)
	}
	if !strings.HasPrefix(s, "# THIS FILE IS AUTOGENERATED") {
		t.Errorf("expected autogen header, got:\n%s", s)
	}
	if len(refs) != 1 || refs[0] != "test" {
		t.Errorf("expected refs=[test], got %v", refs)
	}
}

func TestRenderTemplateRemovesOrphanPlaceholder(t *testing.T) {
	template := []byte("steps:\n  - uses: x\n  # @ci: unknown\n  - run: after\n")
	out, _ := renderTemplate("t.yaml", template, map[string][]Task{})
	s := string(out)
	if strings.Contains(s, "# @ci: unknown") {
		t.Errorf("orphan placeholder should be removed, still present:\n%s", s)
	}
	// The surrounding steps must remain
	if !strings.Contains(s, "- uses: x") || !strings.Contains(s, "- run: after") {
		t.Errorf("surrounding template content damaged:\n%s", s)
	}
}

func TestRenderTemplateSeveralStepsBlankSeparated(t *testing.T) {
	template := []byte("steps:\n  # @ci: t\n")
	tasks := map[string][]Task{
		"t": {
			{Step: "a", Run: "task a"},
			{Step: "b", Run: "task b"},
			{Step: "c", Run: "task c"},
		},
	}
	out, _ := renderTemplate("t.yaml", template, tasks)
	s := string(out)
	for _, want := range []string{
		"  - name: a\n    run: task a",
		"  - name: b\n    run: task b",
		"  - name: c\n    run: task c",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n--- full ---\n%s", want, s)
		}
	}
	// Blank line between consecutive inserted steps.
	if !strings.Contains(s, "run: task a\n\n  - name: b") {
		t.Errorf("expected blank line between consecutive steps:\n%s", s)
	}
}

func TestStripTrailingWhitespace(t *testing.T) {
	in := []byte("foo  \n  bar\t\nbaz\n")
	want := []byte("foo\n  bar\nbaz\n")
	if got := stripTrailingWhitespace(in); !bytes.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

// ---------- in-process unit tests for I/O-touching helpers ----------

func TestStringListAccumulates(t *testing.T) {
	var s stringList
	if err := s.Set("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("b"); err != nil {
		t.Fatal(err)
	}
	if len(s) != 2 || s[0] != "a" || s[1] != "b" {
		t.Errorf("got %v, want [a b]", s)
	}
	if got := s.String(); got != "a,b" {
		t.Errorf("String() = %q, want \"a,b\"", got)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
}

func TestPickInitTemplate(t *testing.T) {
	t.Run("no go.mod -> generic", func(t *testing.T) {
		chdir(t, t.TempDir())
		tmpl, flavor := pickInitTemplate()
		if flavor != "generic" {
			t.Errorf("flavor = %q, want generic", flavor)
		}
		if !strings.Contains(tmpl, "go-task/setup-task@v2") {
			t.Errorf("generic template missing setup-task line:\n%s", tmpl)
		}
	})
	t.Run("with go.mod -> Go", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		chdir(t, dir)
		tmpl, flavor := pickInitTemplate()
		if flavor != "Go" {
			t.Errorf("flavor = %q, want Go", flavor)
		}
		if !strings.Contains(tmpl, "actions/setup-go@v5") {
			t.Errorf("Go template missing setup-go line:\n%s", tmpl)
		}
	})
}

func TestListTemplates(t *testing.T) {
	tmp := t.TempDir()
	// Missing dir -> nil, no error.
	got, err := listTemplates(filepath.Join(tmp, "nope"))
	if err != nil || got != nil {
		t.Errorf("missing dir: got (%v, %v), want (nil, nil)", got, err)
	}
	// Populate.
	for _, name := range []string{"b.yaml", "a.yaml", "z.yml", "ignored.txt"} {
		writeFile(t, filepath.Join(tmp, "t", name), "---\n")
	}
	// Subdir should be skipped.
	if err := os.MkdirAll(filepath.Join(tmp, "t", "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = listTemplates(filepath.Join(tmp, "t"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join(tmp, "t", "a.yaml"),
		filepath.Join(tmp, "t", "b.yaml"),
		filepath.Join(tmp, "t", "z.yml"),
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestIsToolDependency(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if isToolDependency("x.com/foo") {
		t.Error("no go.mod -> should be false")
	}
	if err := os.WriteFile("go.mod", []byte("module x\n\ntool x.com/foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isToolDependency("x.com/foo") {
		t.Error("with tool directive present -> should be true")
	}
	if isToolDependency("x.com/bar") {
		t.Error("different path -> should be false")
	}
}

func TestReadTasks(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "Taskfile.yaml")
	writeFile(t, path, `version: '3'
tasks:
  # @ci: test | Run unit tests
  unit:
    cmd: go test ./...
  # @ci: test
  vet:
    desc: Run go vet
    cmd: go vet ./...
  # @ci: build
  build:
    cmd: go build .
  plain:
    cmd: echo
`)
	got := readTasks(path, "task")
	if len(got["test"]) != 2 {
		t.Errorf("expected 2 tasks for 'test', got %d", len(got["test"]))
	}
	if len(got["build"]) != 1 {
		t.Errorf("expected 1 task for 'build', got %d", len(got["build"]))
	}
	if got["test"][0].Step != "Run unit tests" {
		t.Errorf("annotation override not picked up: %+v", got["test"][0])
	}
	if got["test"][1].Step != "Run go vet" {
		t.Errorf("desc fallback not picked up: %+v", got["test"][1])
	}
	if got["build"][0].Step != "build" {
		t.Errorf("task-name fallback not picked up: %+v", got["build"][0])
	}
	if got["test"][0].Run != "task unit" {
		t.Errorf("run command wrong: %+v", got["test"][0])
	}
	// Unannotated task should not appear under any tag.
	for tag, tasks := range got {
		for _, x := range tasks {
			if x.Name == "plain" {
				t.Errorf("unannotated task 'plain' leaked into tag %q", tag)
			}
		}
	}
}

func TestWarnOrphansBothKinds(t *testing.T) {
	var buf bytes.Buffer
	tasks := map[string][]Task{
		"orphan-tag": {{Name: "x"}},
		"good":       {{Name: "y"}},
	}
	used := map[string]bool{
		"good":             true,
		"orphan-placeholder": true,
	}
	got := warnOrphans(&buf, tasks, used)
	if got != 2 {
		t.Errorf("got %d orphans, want 2", got)
	}
	out := buf.String()
	if !strings.Contains(out, `Tag "orphan-tag"`) {
		t.Errorf("missing orphan-tag warning:\n%s", out)
	}
	if !strings.Contains(out, "# @ci: orphan-tag") {
		t.Errorf("missing paste-snippet for orphan tag:\n%s", out)
	}
	if !strings.Contains(out, "Template placeholder `# @ci: orphan-placeholder`") {
		t.Errorf("missing orphan-placeholder warning:\n%s", out)
	}
}

func TestWarnOrphansSilentWhenClean(t *testing.T) {
	var buf bytes.Buffer
	got := warnOrphans(&buf,
		map[string][]Task{"a": {{Name: "x"}}},
		map[string]bool{"a": true},
	)
	if got != 0 {
		t.Errorf("expected 0 orphans, got %d", got)
	}
	if buf.Len() != 0 {
		t.Errorf("expected silent output, got: %s", buf.String())
	}
}

func TestRunInitWritesGoFlavor(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)
	if err := os.WriteFile("go.mod", []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runInit()
	data, err := os.ReadFile(initDefaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "actions/setup-go@v5") {
		t.Errorf("expected Go template, got:\n%s", data)
	}
}

func TestRunFixRemovesOrphanOnly(t *testing.T) {
	tmp := t.TempDir()
	tpath := filepath.Join(tmp, "ci.yaml")
	writeFile(t, tpath, `steps:
  - uses: x
  # @ci: keep
  # @ci: drop
`)
	tasks := map[string][]Task{"keep": {{Name: "x"}}}
	n := runFix([]string{tpath}, tasks)
	if n != 1 {
		t.Errorf("expected 1 removal, got %d", n)
	}
	data, _ := os.ReadFile(tpath)
	out := string(data)
	if strings.Contains(out, "# @ci: drop") {
		t.Errorf("orphan placeholder still present:\n%s", out)
	}
	if !strings.Contains(out, "# @ci: keep") {
		t.Errorf("non-orphan placeholder was removed:\n%s", out)
	}
}

// ---------- integration (subprocess) ----------

var (
	binaryPath string
	buildOnce  sync.Once
	buildErr   error
)

func buildBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		tmpdir, err := os.MkdirTemp("", "task2ci-bin-")
		if err != nil {
			buildErr = err
			return
		}
		binaryPath = filepath.Join(tmpdir, "task2ci")
		cmd := exec.Command("go", "build", "-o", binaryPath, ".")
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %w\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("failed to build test binary: %v", buildErr)
	}
	return binaryPath
}

func runBinary(t *testing.T, workdir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(buildBinary(t), args...)
	cmd.Dir = workdir
	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("running binary: %v", err)
		}
	}
	return outBuf.String(), errBuf.String(), exitCode
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const integrationTaskfile = `version: '3'
tasks:
  # @ci: build
  build:
    desc: Build the binary
    cmd: go build .
  # @ci: test
  test:
    desc: Run tests
    cmd: go test ./...
  # @ci: test
  vet:
    cmd: go vet ./...
`

const integrationTemplate = `---
name: ci
on: [push, pull_request]
jobs:
  build:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      # @ci: build

  test:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      # @ci: test
`

func TestIntegrationGeneratesWorkflow(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), integrationTaskfile)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), integrationTemplate)

	stdout, stderr, code := runBinary(t, tmp)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%s stdout=%s", code, stderr, stdout)
	}

	data, err := os.ReadFile(filepath.Join(tmp, ".github/workflows/ci.yaml"))
	if err != nil {
		t.Fatalf("expected generated workflow: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"# THIS FILE IS AUTOGENERATED",
		"name: ci",
		"  build:",
		"  test:",
		"runs-on: ubuntu-24.04",
		"- uses: actions/checkout@v4",
		"- name: Build the binary", // desc fallback
		"- name: Run tests",        // desc fallback
		"- name: vet",              // bare task name (no desc)
		"run: task build",          // no go-task tool dep in fixture
		"run: task test",
		"run: task vet",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in generated workflow:\n%s", want, content)
		}
	}
}

func TestIntegrationCheckPassesAfterGeneration(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), integrationTaskfile)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), integrationTemplate)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("generation failed: %s", stderr)
	}
	if _, stderr, code := runBinary(t, tmp, "-check"); code != 0 {
		t.Fatalf("-check failed unexpectedly: %s", stderr)
	}
}

func TestIntegrationCheckDetectsDrift(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), integrationTaskfile)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), integrationTemplate)
	if _, _, code := runBinary(t, tmp); code != 0 {
		t.Fatal("generation failed")
	}
	path := filepath.Join(tmp, ".github/workflows/ci.yaml")
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(data, []byte("# tampered\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runBinary(t, tmp, "-check")
	if code == 0 {
		t.Fatalf("expected -check to fail after tampering; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "drift") {
		t.Errorf("expected drift message, got: %s", stderr)
	}
}

func TestIntegrationCheckFailsOnOrphanPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: ok
  fine:
    cmd: echo
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: ok
      # @ci: nobody
`)
	// Generate first so a workflow exists on disk.
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("plain generate should succeed despite orphan; got %d stderr=%s", code, stderr)
	}
	// -check must fail.
	_, stderr, code := runBinary(t, tmp, "-check")
	if code == 0 {
		t.Fatalf("expected -check to fail on orphan placeholder, got exit 0")
	}
	if !strings.Contains(stderr, "orphan") {
		t.Errorf("expected fatal message to mention orphans, got: %s", stderr)
	}
}

func TestIntegrationCheckFailsOnOrphanTag(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: stray
  loner:
    cmd: echo
  # @ci: ok
  fine:
    cmd: echo
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: ok
`)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("plain generate should succeed; got %d stderr=%s", code, stderr)
	}
	_, stderr, code := runBinary(t, tmp, "-check")
	if code == 0 {
		t.Fatalf("expected -check to fail on orphan tag, got exit 0")
	}
	if !strings.Contains(stderr, "orphan") || !strings.Contains(stderr, "stray") {
		t.Errorf("expected fatal message about orphan tag 'stray', got: %s", stderr)
	}
}

func TestIntegrationOrphanTagWarning(t *testing.T) {
	// Task has tag X but template has no @ci: X placeholder.
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: stray
  loner:
    cmd: echo
  # @ci: ok
  fine:
    cmd: echo
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: ok
`)
	_, stderr, code := runBinary(t, tmp)
	if code != 0 {
		t.Fatalf("generation should still succeed, got exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, `Tag "stray"`) || !strings.Contains(stderr, "no template") {
		t.Errorf("expected orphan-tag warning, got: %s", stderr)
	}
}

func TestIntegrationOrphanPlaceholderWarning(t *testing.T) {
	// Template has @ci: nobody placeholder but no task has that tag.
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: ok
  fine:
    cmd: echo
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: ok
      # @ci: nobody
`)
	_, stderr, code := runBinary(t, tmp)
	if code != 0 {
		t.Fatalf("generation should succeed, got exit=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, `# @ci: nobody`) {
		t.Errorf("expected orphan-placeholder warning, got: %s", stderr)
	}
}

func TestIntegrationStepNameOverride(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: test | Run all unit tests
  test:
    cmd: go test ./...
  # @ci: test
  vet:
    cmd: go vet ./...
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: test
`)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("unexpected failure: %s", stderr)
	}
	data, err := os.ReadFile(filepath.Join(tmp, ".github/workflows/ci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "- name: Run all unit tests") {
		t.Errorf("expected overridden step name 'Run all unit tests':\n%s", content)
	}
	if !strings.Contains(content, "run: task test") {
		t.Errorf("expected run line to use task name:\n%s", content)
	}
	if !strings.Contains(content, "- name: vet") {
		t.Errorf("expected vet step to use task name (no override, no desc):\n%s", content)
	}
}

func TestIntegrationMultipleTemplates(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: build
  build:
    cmd: go build
  # @ci: release
  release:
    cmd: goreleaser
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: build
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/release.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: release
`)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("unexpected failure: %s", stderr)
	}
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		if _, err := os.Stat(filepath.Join(tmp, ".github/workflows", name)); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
	}
}

func TestIntegrationAcceptsYmlExtension(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: build
  build:
    cmd: go build
`)
	// Template uses .yml extension.
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: build
`)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("unexpected failure: %s", stderr)
	}
	// Output should still be .yaml regardless of source extension.
	if _, err := os.Stat(filepath.Join(tmp, ".github/workflows/ci.yaml")); err != nil {
		t.Errorf("expected .yaml output even with .yml template: %v", err)
	}
}

func TestFindTaskfileResolutionOrder(t *testing.T) {
	tmp := t.TempDir()
	oldwd, _ := os.Getwd()
	defer os.Chdir(oldwd) //nolint:errcheck
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}
	// No files: returns "".
	if got := findTaskfile(); got != "" {
		t.Errorf("empty dir: want \"\", got %q", got)
	}
	// Lower-precedence candidate only.
	if err := os.WriteFile("Taskfile.yaml", []byte("version: '3'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findTaskfile(); got != "Taskfile.yaml" {
		t.Errorf("only Taskfile.yaml present: want Taskfile.yaml, got %q", got)
	}
	// Higher-precedence candidate beats lower.
	if err := os.WriteFile("Taskfile.yml", []byte("version: '3'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := findTaskfile(); got != "Taskfile.yml" {
		t.Errorf("Taskfile.yml present alongside Taskfile.yaml: want Taskfile.yml, got %q", got)
	}
}

func TestIntegrationFindsTaskfileYml(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yml"), integrationTaskfile)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), integrationTemplate)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("expected success with Taskfile.yml, got code=%d stderr=%s", code, stderr)
	}
}

func TestIntegrationTaskfileFlagOverride(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "MyTaskfile.yaml"), integrationTaskfile)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), integrationTemplate)
	if _, stderr, code := runBinary(t, tmp, "-taskfile", "MyTaskfile.yaml"); code != 0 {
		t.Fatalf("expected -taskfile override to succeed, got code=%d stderr=%s", code, stderr)
	}
}

func TestIntegrationMultipleTaskfiles(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "a.yaml"), `version: '3'
tasks:
  # @ci: test
  unit:
    cmd: go test ./...
`)
	writeFile(t, filepath.Join(tmp, "b.yaml"), `version: '3'
tasks:
  # @ci: test
  integration:
    cmd: go test -tags=integration ./...
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: test
`)
	if _, stderr, code := runBinary(t, tmp, "-taskfile", "a.yaml", "-taskfile", "b.yaml"); code != 0 {
		t.Fatalf("expected success, got code=%d stderr=%s", code, stderr)
	}
	data, err := os.ReadFile(filepath.Join(tmp, ".github/workflows/ci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{
		"- name: unit\n        run: task unit",
		"- name: integration\n        run: task integration",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("expected %q in output:\n%s", want, content)
		}
	}
}

func TestIntegrationDuplicateTaskAcrossFilesWarns(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "a.yaml"), `version: '3'
tasks:
  # @ci: test
  shared:
    cmd: echo a
`)
	writeFile(t, filepath.Join(tmp, "b.yaml"), `version: '3'
tasks:
  # @ci: test
  shared:
    cmd: echo b
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      # @ci: test
`)
	_, stderr, code := runBinary(t, tmp, "-taskfile", "a.yaml", "-taskfile", "b.yaml")
	if code != 0 {
		t.Fatalf("expected success despite warning, got code=%d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, `Task "shared"`) {
		t.Errorf("expected duplicate-task warning, got: %s", stderr)
	}
}

func TestIntegrationTaskfileFlagMissingFileIsFatal(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), integrationTemplate)
	_, stderr, code := runBinary(t, tmp, "-taskfile", "does-not-exist.yaml")
	if code == 0 {
		t.Fatalf("expected non-zero exit on missing -taskfile target")
	}
	if !strings.Contains(stderr, "does-not-exist.yaml") {
		t.Errorf("expected error to mention the missing path, got: %s", stderr)
	}
}

func TestIntegrationNoTaskfileIsFatal(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), integrationTemplate)
	_, stderr, code := runBinary(t, tmp)
	if code == 0 {
		t.Fatalf("expected non-zero exit when no Taskfile present")
	}
	if !strings.Contains(stderr, "No Taskfile found") {
		t.Errorf("expected helpful message, got: %s", stderr)
	}
}

func TestIntegrationLicenseFlag(t *testing.T) {
	tmp := t.TempDir()
	stdout, stderr, code := runBinary(t, tmp, "-license")
	if code != 0 {
		t.Fatalf("-license should exit 0, got code=%d stderr=%s", code, stderr)
	}
	for _, want := range []string{
		"MIT License",
		"Copyright (c) 2026 Arne Jørgensen",
		"THE SOFTWARE IS PROVIDED \"AS IS\"",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("license output missing %q\n%s", want, stdout)
		}
	}
	// -license should short-circuit: works even when no Taskfile or
	// template is present in cwd (no fatal "No Taskfile found").
	if strings.Contains(stderr, "No Taskfile") {
		t.Errorf("-license should short-circuit, not require a Taskfile; stderr=%s", stderr)
	}
}

func TestIntegrationInitGoFlavorWithGoMod(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "go.mod"), "module example.com/x\n\ngo 1.24\n")
	stdout, stderr, code := runBinary(t, tmp, "-init")
	if code != 0 {
		t.Fatalf("-init failed: code=%d stderr=%s stdout=%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "Go starter") {
		t.Errorf("expected 'Go starter' in stdout, got: %s", stdout)
	}
	path := filepath.Join(tmp, ".task2ci/workflows/ci.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected template at %s: %v", path, err)
	}
	content := string(data)
	for _, want := range []string{
		"name: ci",
		"runs-on: ubuntu-24.04",
		"actions/checkout@v4",
		"actions/setup-go@v5",
		"go tool task2ci -check",
		"# @ci: test",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("Go init template missing %q\n%s", want, content)
		}
	}
	if strings.Contains(content, "go-task/setup-task") {
		t.Errorf("Go template should not include setup-task (go-task is via go tool):\n%s", content)
	}
}

func TestIntegrationInitGenericFlavorWithoutGoMod(t *testing.T) {
	tmp := t.TempDir()
	stdout, stderr, code := runBinary(t, tmp, "-init")
	if code != 0 {
		t.Fatalf("-init failed: code=%d stderr=%s stdout=%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "generic starter") {
		t.Errorf("expected 'generic starter' in stdout, got: %s", stdout)
	}
	path := filepath.Join(tmp, ".task2ci/workflows/ci.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected template at %s: %v", path, err)
	}
	content := string(data)
	for _, want := range []string{
		"name: ci",
		"runs-on: ubuntu-24.04",
		"actions/checkout@v4",
		"go-task/setup-task@v2",
		"go run arnested.dk/go/task2ci@latest -check",
		"# @ci: test",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("generic init template missing %q\n%s", want, content)
		}
	}
	if strings.Contains(content, "uses: actions/setup-go") {
		t.Errorf("generic template should not USE actions/setup-go (a comment may mention it):\n%s", content)
	}
	if strings.Contains(content, "go install arnested.dk/go/task2ci") {
		t.Errorf("generic template should use `go run @latest`, not a separate `go install` step:\n%s", content)
	}
	if strings.Contains(content, "go tool task2ci") {
		t.Errorf("generic template should use `go run` (no tool-dep assumed), not `go tool task2ci`:\n%s", content)
	}
}

func TestIntegrationInitRefusesToOverwrite(t *testing.T) {
	tmp := t.TempDir()
	existing := "---\nname: pre-existing\n"
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), existing)
	_, stderr, code := runBinary(t, tmp, "-init")
	if code == 0 {
		t.Fatalf("-init should refuse to overwrite, got exit 0")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("expected refusal message, got: %s", stderr)
	}
	data, _ := os.ReadFile(filepath.Join(tmp, ".task2ci/workflows/ci.yaml"))
	if string(data) != existing {
		t.Errorf("existing template was clobbered; got: %s", data)
	}
}

func TestIntegrationFixRemovesOrphanPlaceholder(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: ok
  fine:
    cmd: echo
`)
	tpath := filepath.Join(tmp, ".task2ci/workflows/ci.yaml")
	writeFile(t, tpath, `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      # @ci: ok
      # @ci: ghost
      - name: tail
        run: echo
`)
	stdout, stderr, code := runBinary(t, tmp, "-fix")
	if code != 0 {
		t.Fatalf("-fix failed: code=%d stderr=%s stdout=%s", code, stderr, stdout)
	}
	data, err := os.ReadFile(tpath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if strings.Contains(content, "# @ci: ghost") {
		t.Errorf("ghost placeholder should be removed:\n%s", content)
	}
	if !strings.Contains(content, "# @ci: ok") {
		t.Errorf("non-orphan placeholder should be preserved:\n%s", content)
	}
	if !strings.Contains(content, "- uses: actions/checkout@v4") || !strings.Contains(content, "name: tail") {
		t.Errorf("surrounding template damaged:\n%s", content)
	}
}

func TestIntegrationFixIsIdempotent(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: ok
  fine:
    cmd: echo
`)
	tpath := filepath.Join(tmp, ".task2ci/workflows/ci.yaml")
	writeFile(t, tpath, `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
      # @ci: ok
`)
	if _, stderr, code := runBinary(t, tmp, "-fix"); code != 0 {
		t.Fatalf("-fix on already-clean template should succeed: %s", stderr)
	}
	before, _ := os.ReadFile(tpath)
	if _, _, code := runBinary(t, tmp, "-fix"); code != 0 {
		t.Fatal("second -fix run should also succeed")
	}
	after, _ := os.ReadFile(tpath)
	if !bytes.Equal(before, after) {
		t.Errorf("second -fix should be a no-op; got diff:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestIntegrationFlagsAreMutuallyExclusive(t *testing.T) {
	tmp := t.TempDir()
	_, stderr, code := runBinary(t, tmp, "-check", "-fix")
	if code == 0 {
		t.Fatalf("expected non-zero exit when -check and -fix combined")
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Errorf("expected mutual-exclusion message, got: %s", stderr)
	}
}

func TestIntegrationOrphanTagWarningIncludesSnippet(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: forgotten
  lonely:
    cmd: echo
`)
	writeFile(t, filepath.Join(tmp, ".task2ci/workflows/ci.yaml"), `---
jobs:
  j:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/checkout@v4
`)
	_, stderr, code := runBinary(t, tmp)
	if code != 0 {
		t.Fatalf("plain generate should succeed: %s", stderr)
	}
	// Snippet line for paste must appear
	if !strings.Contains(stderr, "# @ci: forgotten") {
		t.Errorf("expected paste-able placeholder in stderr, got: %s", stderr)
	}
}

func TestIntegrationNoTemplatesIsFatal(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  noop:
    cmd: echo
`)
	_, stderr, code := runBinary(t, tmp)
	if code == 0 {
		t.Fatalf("expected non-zero exit when no templates exist, got 0")
	}
	if !strings.Contains(stderr, "No templates found") {
		t.Errorf("expected helpful message, got: %s", stderr)
	}
}
