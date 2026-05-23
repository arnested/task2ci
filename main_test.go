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

func TestOutputPath(t *testing.T) {
	want := filepath.Join(".github", "workflows", "ci.yaml")
	if got := outputPath("ci"); got != want {
		t.Errorf("outputPath(\"ci\") = %q, want %q", got, want)
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
		{
			"present",
			"desc: build the thing\ncmd: go build\n",
			"build the thing",
		},
		{
			"absent",
			"cmd: go test\n",
			"",
		},
		{
			"present with other fields",
			"cmd: go test\ndesc: run tests\nvars:\n  X: 1\n",
			"run tests",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(c.yamlSrc), &doc); err != nil {
				t.Fatal(err)
			}
			mapping := doc.Content[0]
			if got := taskDesc(mapping); got != c.want {
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
	src := `
version: '3'
tasks:
  build:
    cmd: go build
  test:
    cmd: go test
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	node := findTasksNode(&doc)
	if node == nil {
		t.Fatal("findTasksNode returned nil for a Taskfile with a tasks block")
	}
	if node.Kind != yaml.MappingNode {
		t.Errorf("expected MappingNode, got kind %v", node.Kind)
	}
	if len(node.Content) != 4 { // two key/value pairs
		t.Errorf("expected 4 children (2 tasks × key+value), got %d", len(node.Content))
	}
}

func TestFindTasksNodeAbsent(t *testing.T) {
	src := "version: '3'\nincludes:\n  other: ./Other.yml\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(src), &doc); err != nil {
		t.Fatal(err)
	}
	if node := findTasksNode(&doc); node != nil {
		t.Errorf("expected nil when no tasks block, got %+v", node)
	}
}

// ---------- yaml marshaling ----------

func TestStepMarshalYAMLPrependsNewline(t *testing.T) {
	s := Step{Name: "first", Run: "echo hi"}
	v, err := s.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	node := v.(*yaml.Node)
	if !strings.HasPrefix(node.HeadComment, "\n") {
		t.Errorf("HeadComment should start with newline (for blank-line separation), got %q", node.HeadComment)
	}
}

func TestJobMarshalYAMLOrdering(t *testing.T) {
	j := Job{
		RunsOn: "ubuntu-latest",
		Steps: []Step{
			{Uses: "actions/checkout@v4"},
			{Name: "test", Run: "go test ./..."},
		},
	}
	v, err := j.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	node := v.(*yaml.Node)
	if node.Kind != yaml.MappingNode {
		t.Fatalf("expected MappingNode, got %v", node.Kind)
	}
	// Expect runs-on first, steps second.
	if node.Content[0].Value != "runs-on" {
		t.Errorf("first key = %q, want runs-on", node.Content[0].Value)
	}
	if node.Content[2].Value != "steps" {
		t.Errorf("third key = %q, want steps", node.Content[2].Value)
	}
}

func TestJobMarshalYAMLFirstStepHasNoBlankLine(t *testing.T) {
	j := Job{
		RunsOn: "ubuntu-latest",
		Steps:  []Step{{Uses: "actions/checkout@v4"}, {Name: "x", Run: "x"}},
	}
	v, _ := j.MarshalYAML()
	node := v.(*yaml.Node)
	stepsNode := node.Content[3]
	first := stepsNode.Content[0]
	if strings.HasPrefix(first.HeadComment, "\n") {
		t.Errorf("first step should not have leading newline in HeadComment, got %q", first.HeadComment)
	}
	second := stepsNode.Content[1]
	if !strings.HasPrefix(second.HeadComment, "\n") {
		t.Errorf("second step should have leading newline, got %q", second.HeadComment)
	}
}

func TestWorkflowEncodeStructure(t *testing.T) {
	w := Workflow{
		Name: "ci",
		On:   []string{"push", "pull_request"},
		Jobs: Jobs{
			"test": Job{
				RunsOn: "ubuntu-latest",
				Steps: []Step{
					{Uses: "actions/checkout@v4"},
					{Name: "Run tests", Run: "go test ./..."},
				},
			},
		},
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&w); err != nil {
		t.Fatal(err)
	}
	out := buf.String()

	for _, want := range []string{
		"name: ci",
		"on:\n",
		"  - push",
		"  - pull_request",
		"jobs:",
		"  test:",
		"    runs-on: ubuntu-latest",
		"      - uses: actions/checkout@v4",
		"      - name: Run tests",
		"        run: go test ./...",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull output:\n%s", want, out)
		}
	}

	// Blank line between top-level keys (after `name: ci`, before `on:`)
	if !strings.Contains(out, "name: ci\n\non:") {
		t.Error("expected blank line between name and on")
	}
}

func TestJobsMarshalYAMLSortsKeysAndBlankLineBetween(t *testing.T) {
	js := Jobs{
		"zebra": Job{RunsOn: "u", Steps: []Step{{Uses: "x"}}},
		"alpha": Job{RunsOn: "u", Steps: []Step{{Uses: "y"}}},
	}
	v, err := js.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	node := v.(*yaml.Node)
	if node.Content[0].Value != "alpha" {
		t.Errorf("first job = %q, want alpha (sorted)", node.Content[0].Value)
	}
	if node.Content[2].Value != "zebra" {
		t.Errorf("second job = %q, want zebra", node.Content[2].Value)
	}
	if node.Content[0].HeadComment != "" {
		t.Errorf("first job should have no HeadComment, got %q", node.Content[0].HeadComment)
	}
	if node.Content[2].HeadComment != "\n" {
		t.Errorf("second job should have HeadComment=\"\\n\", got %q", node.Content[2].HeadComment)
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
	exitCode = 0
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
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
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

const schemaRef = "# yaml-language-server: $schema=https://arnested.dk/schemas/task2ci.schema.json\n"

const integrationConfig = schemaRef + `tags:
  build:
    workflow: ci
    runs-on: ubuntu-latest
  test:
    workflow: ci
    runs-on: ubuntu-latest
`

func TestIntegrationGeneratesWorkflow(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), integrationTaskfile)
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), integrationConfig)

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
		"    runs-on: ubuntu-latest",
		"- name: Build the binary", // desc-as-step-name fallback
		"- name: Run tests",        // desc-as-step-name fallback
		"- name: vet",              // no desc, no override → task name
		"uses: actions/checkout@v4",
		"uses: arnested/setup-task2ci@v1",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("generated workflow missing %q\n--- full output ---\n%s", want, content)
		}
	}
}

func TestIntegrationCheckPassesAfterGeneration(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), integrationTaskfile)
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), integrationConfig)

	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("generation failed: code=%d stderr=%s", code, stderr)
	}
	stdout, stderr, code := runBinary(t, tmp, "-check")
	if code != 0 {
		t.Fatalf("-check failed unexpectedly: code=%d stderr=%s stdout=%s", code, stderr, stdout)
	}
}

func TestIntegrationCheckDetectsDrift(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), integrationTaskfile)
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), integrationConfig)

	if _, _, code := runBinary(t, tmp); code != 0 {
		t.Fatal("generation failed")
	}
	// Tamper with the generated file
	path := filepath.Join(tmp, ".github/workflows/ci.yaml")
	data, _ := os.ReadFile(path)
	if err := os.WriteFile(path, append(data, []byte("# tampered\n")...), 0644); err != nil {
		t.Fatal(err)
	}
	_, stderr, code := runBinary(t, tmp, "-check")
	if code == 0 {
		t.Fatalf("expected -check to fail after tampering, but it passed; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "drift") {
		t.Errorf("expected stderr to mention drift, got: %s", stderr)
	}
}

func TestIntegrationUnknownTagWarns(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: nope
  rogue:
    cmd: echo
`)
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), schemaRef+`tags: {}`)

	_, stderr, code := runBinary(t, tmp)
	if code != 0 {
		t.Fatalf("unexpected non-zero exit: %d stderr=%s", code, stderr)
	}
	if !strings.Contains(stderr, "unknown tag") {
		t.Errorf("expected unknown-tag warning, stderr=%s", stderr)
	}
}

func TestIntegrationConflictingRunsOnIsFatal(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: a
  x:
    cmd: echo
`)
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), schemaRef+`tags:
  a:
    workflow: ci
    job: shared
    runs-on: ubuntu-latest
  b:
    workflow: ci
    job: shared
    runs-on: macos-latest
`)
	_, stderr, code := runBinary(t, tmp)
	if code == 0 {
		t.Errorf("expected non-zero exit for conflicting runs-on, got 0; stderr=%s", stderr)
	}
	if !strings.Contains(stderr, "different runs-on") {
		t.Errorf("expected runs-on conflict message, stderr=%s", stderr)
	}
}

func TestIntegrationDefaultsWorkflowAndJob(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: only
  do:
    cmd: echo
`)
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), schemaRef+`tags:
  only:
    runs-on: ubuntu-latest
`)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("unexpected failure: %s", stderr)
	}
	// No workflow → defaults to "taskfile"; no job → defaults to tag name "only"
	data, err := os.ReadFile(filepath.Join(tmp, ".github/workflows/taskfile.yaml"))
	if err != nil {
		t.Fatalf("expected taskfile.yaml: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "  only:") {
		t.Errorf("expected job named after tag (only), got:\n%s", content)
	}
}

func TestIntegrationMultipleWorkflows(t *testing.T) {
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
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), schemaRef+`tags:
  build:
    workflow: ci
    runs-on: ubuntu-latest
  release:
    workflow: release
    runs-on: ubuntu-latest
`)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("unexpected failure: %s", stderr)
	}
	for _, name := range []string{"ci.yaml", "release.yaml"} {
		path := filepath.Join(tmp, ".github/workflows", name)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected %s to exist: %v", name, err)
		}
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
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), schemaRef+`tags:
  test:
    workflow: ci
    runs-on: ubuntu-latest
`)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("unexpected failure: %s", stderr)
	}
	data, err := os.ReadFile(filepath.Join(tmp, ".github/workflows/ci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	// Annotated step gets the override name; the run target stays the task name.
	if !strings.Contains(content, "- name: Run all unit tests") {
		t.Errorf("expected overridden step name 'Run all unit tests', got:\n%s", content)
	}
	if !strings.Contains(content, "run: task test") {
		t.Errorf("expected run line to use task name, got:\n%s", content)
	}
	// Unannotated step still uses the task name.
	if !strings.Contains(content, "- name: vet") {
		t.Errorf("expected vet step to use task name as step name, got:\n%s", content)
	}
}

func TestIntegrationCheckValidatesSchema(t *testing.T) {
	cases := []struct {
		name, config, wantErrFragment string
	}{
		{
			"missing runs-on",
			"---\ntags:\n  x:\n    workflow: ci\n",
			"missing property 'runs-on'",
		},
		{
			"unknown top-level key",
			"---\nweird: true\ntags: {}\n",
			"additional properties 'weird' not allowed",
		},
		{
			"tag value is a scalar",
			"---\ntags:\n  shorthand: ubuntu-latest\n",
			"got string, want object",
		},
		{
			"unknown actions key",
			"---\nactions:\n  setup-ruby: ruby/setup-ruby@v1\ntags: {}\n",
			"additional properties 'setup-ruby' not allowed",
		},
		{
			"checkout malformed (missing @ref)",
			"---\nactions:\n  checkout: actions-checkout-v4\ntags: {}\n",
			"does not match pattern",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tmp := t.TempDir()
			writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  noop:
    cmd: echo
`)
			writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), c.config)

			// First generate so -check has a workflow file to compare against.
			// (Schema validation should fire before drift check, but generation
			// itself uses loadConfig which doesn't run schema validation.)
			_, _, _ = runBinary(t, tmp)

			_, stderr, code := runBinary(t, tmp, "-check")
			if code == 0 {
				t.Fatalf("expected -check to fail on invalid config, exit was 0\nstderr=%s", stderr)
			}
			if !strings.Contains(stderr, "does not conform to the schema") {
				t.Errorf("expected schema-violation message, got: %s", stderr)
			}
			if !strings.Contains(stderr, c.wantErrFragment) {
				t.Errorf("expected error fragment %q, got: %s", c.wantErrFragment, stderr)
			}
		})
	}
}

func TestIntegrationCheckPassesSchemaOnValidConfig(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), integrationTaskfile)
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), integrationConfig)

	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("generation failed: %s", stderr)
	}
	if _, stderr, code := runBinary(t, tmp, "-check"); code != 0 {
		t.Fatalf("-check failed on valid config: %s", stderr)
	}
}

func TestIntegrationInitWritesStarterConfig(t *testing.T) {
	tmp := t.TempDir()

	_, stderr, code := runBinary(t, tmp, "-init")
	if code != 0 {
		t.Fatalf("-init failed: code=%d stderr=%s", code, stderr)
	}

	data, err := os.ReadFile(filepath.Join(tmp, ".task2ci.yaml"))
	if err != nil {
		t.Fatalf("expected .task2ci.yaml to be written: %v", err)
	}
	content := string(data)
	for _, want := range []string{
		"yaml-language-server:",
		"actions:",
		"checkout: actions/checkout@v4",
		"setup-task: go-task/setup-task@v2",
		"setup-task2ci: arnested/setup-task2ci@v1",
		"tags:",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("starter config missing %q\n%s", want, content)
		}
	}
}

func TestIntegrationInitRefusesToOverwrite(t *testing.T) {
	tmp := t.TempDir()
	existing := "tags: {}\n"
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), existing)

	_, stderr, code := runBinary(t, tmp, "-init")
	if code == 0 {
		t.Fatalf("-init should fail when config already exists, got exit 0")
	}
	if !strings.Contains(stderr, "already exists") {
		t.Errorf("expected stderr to mention existing file, got: %s", stderr)
	}

	data, err := os.ReadFile(filepath.Join(tmp, ".task2ci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != existing {
		t.Errorf("existing config was clobbered; got: %s", data)
	}
}

func TestIntegrationInitProducesValidConfig(t *testing.T) {
	// The starter must parse and conform to the schema (i.e., loadConfig accepts it
	// and a subsequent generation runs without fatal config errors). We test the
	// loadConfig accept side here; schema conformance is enforced by yajsv in CI.
	tmp := t.TempDir()
	if _, stderr, code := runBinary(t, tmp, "-init"); code != 0 {
		t.Fatalf("-init failed: %s", stderr)
	}
	// A Taskfile with no @ci annotations: generator runs cleanly but emits 0 files.
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  noop:
    cmd: echo
`)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("generator failed against init config: %s", stderr)
	}
}

func TestIntegrationActionOverrides(t *testing.T) {
	tmp := t.TempDir()
	writeFile(t, filepath.Join(tmp, "Taskfile.yaml"), `version: '3'
tasks:
  # @ci: test
  test:
    cmd: go test
`)
	writeFile(t, filepath.Join(tmp, ".task2ci.yaml"), schemaRef+`actions:
  checkout: actions/checkout@v5
  setup-task: my-fork/setup-task@v9
  setup-task2ci: my-fork/setup-task2ci@v9
tags:
  test:
    workflow: ci
    runs-on: ubuntu-latest
`)
	if _, stderr, code := runBinary(t, tmp); code != 0 {
		t.Fatalf("unexpected failure: %s", stderr)
	}
	data, err := os.ReadFile(filepath.Join(tmp, ".github/workflows/ci.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	for _, want := range []string{
		"actions/checkout@v5",
		"my-fork/setup-task@v9",
		"my-fork/setup-task2ci@v9",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("missing %q in:\n%s", want, content)
		}
	}
}
