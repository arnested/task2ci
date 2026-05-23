package main

import (
	"bytes"
	_ "embed"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"
)

//go:embed task2ci.schema.json
var configSchema []byte

// Config is the user-supplied configuration loaded from .task2ci.yml.
type Config struct {
	Actions Actions            `yaml:"actions"`
	Tags    map[string]TagSpec `yaml:"tags"`
}

// Actions holds global overrides for action references used in every job.
type Actions struct {
	Checkout     string `yaml:"checkout"`
	SetupTask    string `yaml:"setup-task"`
	SetupTask2CI string `yaml:"setup-task2ci"`
}

type TagSpec struct {
	Workflow string `yaml:"workflow"`
	Job      string `yaml:"job"`
	RunsOn   string `yaml:"runs-on"`
}

// GitHub Actions YAML Structs
type Workflow struct {
	Name string
	On   []string
	Jobs Jobs
}

func (w Workflow) MarshalYAML() (interface{}, error) {
	nameVal := &yaml.Node{Kind: yaml.ScalarNode, Value: w.Name}

	onVal := &yaml.Node{Kind: yaml.SequenceNode}
	for _, s := range w.On {
		onVal.Content = append(onVal.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: s})
	}

	jobsValIface, err := w.Jobs.MarshalYAML()
	if err != nil {
		return nil, err
	}
	jobsVal := jobsValIface.(*yaml.Node)

	out := &yaml.Node{Kind: yaml.MappingNode}
	out.Content = append(out.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "name"}, nameVal,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "on", HeadComment: "\n"}, onVal,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "jobs", HeadComment: "\n"}, jobsVal,
	)
	return out, nil
}

// Jobs marshals to a YAML mapping with a blank line between each job.
// Each Job's value is built by Job.MarshalYAML directly so that head comments
// on nested Steps survive (yaml.Node.Encode strips HeadComment from values
// returned by MarshalYAML on nested types).
type Jobs map[string]Job

func (j Jobs) MarshalYAML() (interface{}, error) {
	keys := make([]string, 0, len(j))
	for k := range j {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := &yaml.Node{Kind: yaml.MappingNode}
	for i, k := range keys {
		keyNode := &yaml.Node{Kind: yaml.ScalarNode, Value: k}
		if i > 0 {
			keyNode.HeadComment = "\n"
		}
		v, err := j[k].MarshalYAML()
		if err != nil {
			return nil, err
		}
		out.Content = append(out.Content, keyNode, v.(*yaml.Node))
	}
	return out, nil
}

type Job struct {
	RunsOn string
	Steps  []Step
}

func (j Job) MarshalYAML() (interface{}, error) {
	stepsNode := &yaml.Node{Kind: yaml.SequenceNode}
	for i, s := range j.Steps {
		v, err := s.MarshalYAML()
		if err != nil {
			return nil, err
		}
		stepNode := v.(*yaml.Node)
		if i == 0 {
			// No blank line between `steps:` and the first step.
			stepNode.HeadComment = strings.TrimPrefix(stepNode.HeadComment, "\n")
		}
		stepsNode.Content = append(stepsNode.Content, stepNode)
	}
	out := &yaml.Node{Kind: yaml.MappingNode}
	out.Content = append(out.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: "runs-on"},
		&yaml.Node{Kind: yaml.ScalarNode, Value: j.RunsOn},
		&yaml.Node{Kind: yaml.ScalarNode, Value: "steps"},
		stepsNode,
	)
	return out, nil
}

type Step struct {
	Name string
	Uses string
	Run  string
}

func (s Step) MarshalYAML() (interface{}, error) {
	type stepFields struct {
		Name string `yaml:"name,omitempty"`
		Uses string `yaml:"uses,omitempty"`
		Run  string `yaml:"run,omitempty"`
	}
	var node yaml.Node
	if err := node.Encode(stepFields(s)); err != nil {
		return nil, err
	}
	// Leading "\n" makes yaml.v3 emit a blank line before the step.
	node.HeadComment = "\n"
	return &node, nil
}

const (
	OutputDir       = ".github/workflows"
	ConfigFile      = ".task2ci.yml"
	DefaultWorkflow = "taskfile"
	DefaultCheckout     = "actions/checkout@v4"
	DefaultSetupTask    = "go-task/setup-task@v2"
	DefaultSetupTask2CI = "arnested/setup-task2ci@v1"
	ModulePath          = "arnested.dk/go/task2ci"
	GoTaskToolPath      = "github.com/go-task/task/v3/cmd/task"
)

func outputPath(workflow string) string {
	return filepath.Join(OutputDir, workflow+".yml")
}

// stripTrailingWhitespace removes trailing spaces/tabs from each line.
// yaml.v3 emits the current indent on blank lines, which yamllint flags.
func stripTrailingWhitespace(b []byte) []byte {
	lines := bytes.Split(b, []byte("\n"))
	for i, line := range lines {
		lines[i] = bytes.TrimRight(line, " \t")
	}
	return bytes.Join(lines, []byte("\n"))
}

// loadConfig reads .task2ci.yml. A missing file yields an empty config; any
// task referencing a tag will warn. Inconsistent runs-on across tags pointing
// to the same job is reported but non-fatal (first tag wins).
func loadConfig() *Config {
	data, err := os.ReadFile(ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{
				Actions: Actions{
					Checkout:     DefaultCheckout,
					SetupTask:    DefaultSetupTask,
					SetupTask2CI: DefaultSetupTask2CI,
				},
				Tags: map[string]TagSpec{},
			}
		}
		log.Fatalf("Error reading %s: %v", ConfigFile, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		log.Fatalf("Error parsing %s: %v", ConfigFile, err)
	}
	if cfg.Tags == nil {
		cfg.Tags = map[string]TagSpec{}
	}
	if cfg.Actions.Checkout == "" {
		cfg.Actions.Checkout = DefaultCheckout
	}
	if cfg.Actions.SetupTask == "" {
		cfg.Actions.SetupTask = DefaultSetupTask
	}
	if cfg.Actions.SetupTask2CI == "" {
		cfg.Actions.SetupTask2CI = DefaultSetupTask2CI
	}
	// Validate: tags targeting the same (workflow, job) must share runs-on.
	type jobKey struct{ workflow, job string }
	byJob := map[jobKey]struct {
		tag    string
		runsOn string
	}{}
	tagNames := make([]string, 0, len(cfg.Tags))
	for tag := range cfg.Tags {
		tagNames = append(tagNames, tag)
	}
	sort.Strings(tagNames)
	for _, tag := range tagNames {
		spec := cfg.Tags[tag]
		if spec.Workflow == "" {
			spec.Workflow = DefaultWorkflow
		}
		if spec.Job == "" {
			spec.Job = tag
		}
		cfg.Tags[tag] = spec
		if spec.RunsOn == "" {
			log.Fatalf("%s: tag %q is missing required 'runs-on' field", ConfigFile, tag)
		}
		key := jobKey{spec.Workflow, spec.Job}
		if prev, ok := byJob[key]; ok && prev.runsOn != spec.RunsOn {
			log.Fatalf("%s: tags %q and %q both target job %q in workflow %q but specify different runs-on (%s vs %s)",
				ConfigFile, prev.tag, tag, spec.Job, spec.Workflow, prev.runsOn, spec.RunsOn)
		}
		if _, ok := byJob[key]; !ok {
			byJob[key] = struct {
				tag    string
				runsOn string
			}{tag, spec.RunsOn}
		}
	}
	return &cfg
}

// isToolDependency reports whether the current directory's go.mod registers
// the given module path as a Go tool dependency. When true, the generated
// workflow can invoke the binary via `go tool <name>` without a separate
// install step.
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
		// Strip line comments.
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

const initTemplate = `# yaml-language-server: $schema=https://arnested.dk/schemas/task2ci.schema.json
---
actions:
  checkout: actions/checkout@v4
  setup-task: go-task/setup-task@v2
  setup-task2ci: arnested/setup-task2ci@v1

tags:
  # Define a tag here, then reference it from Taskfile.yml as e.g.
  #   # @ci: test
  # on the tasks you want included in the generated workflow.
  #
  # Example:
  # test:
  #   workflow: ci          # output file: .github/workflows/ci.yml (default: taskfile)
  #   job: test             # job name within the workflow (default: tag name)
  #   runs-on: ubuntu-latest
`

// validateConfigAgainstSchema validates the .task2ci.yml on disk against the
// embedded JSON Schema. Fatal on schema failure. Missing config file is OK
// (treated as empty / fully default).
func validateConfigAgainstSchema() {
	data, err := os.ReadFile(ConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Fatalf("Error reading %s: %v", ConfigFile, err)
	}
	var doc any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		log.Fatalf("Error parsing %s: %v", ConfigFile, err)
	}

	schemaDoc, err := jsonschema.UnmarshalJSON(bytes.NewReader(configSchema))
	if err != nil {
		log.Fatalf("Embedded schema is malformed JSON: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("task2ci.schema.json", schemaDoc); err != nil {
		log.Fatalf("Failed to register embedded schema: %v", err)
	}
	sch, err := c.Compile("task2ci.schema.json")
	if err != nil {
		log.Fatalf("Embedded schema is invalid: %v", err)
	}
	if err := sch.Validate(doc); err != nil {
		log.Fatalf("❌ ERROR: %s does not conform to the schema:\n%v", ConfigFile, err)
	}
}

// writeInitConfig writes a starter .task2ci.yml. Refuses to overwrite if one exists.
func writeInitConfig() {
	if _, err := os.Stat(ConfigFile); err == nil {
		log.Fatalf("%s already exists; refusing to overwrite", ConfigFile)
	} else if !os.IsNotExist(err) {
		log.Fatalf("Could not stat %s: %v", ConfigFile, err)
	}
	if err := os.WriteFile(ConfigFile, []byte(initTemplate), 0644); err != nil {
		log.Fatalf("Error writing %s: %v", ConfigFile, err)
	}
	fmt.Printf("✅ Wrote starter %s. Edit it to define your tags, then annotate Taskfile tasks with `# @ci: <tag>`.\n", ConfigFile)
}

func main() {
	checkPtr := flag.Bool("check", false, "Fail if the generated action is not up to date with Taskfile")
	initPtr := flag.Bool("init", false, "Write a starter "+ConfigFile+" and exit")
	flag.Parse()

	if *initPtr {
		writeInitConfig()
		return
	}

	if *checkPtr {
		validateConfigAgainstSchema()
	}

	task2ciAsTool := isToolDependency(ModulePath)
	goTaskAsTool := isToolDependency(GoTaskToolPath)
	taskCmd := "task"
	if goTaskAsTool {
		taskCmd = "go tool task"
	}

	cfg := loadConfig()

	// 1. Read the Taskfile
	taskfileData, err := os.ReadFile("Taskfile.yml")
	if err != nil {
		log.Fatalf("Error reading Taskfile.yml: %v", err)
	}

	// 2. Parse YAML into an AST to preserve comments
	var root yaml.Node
	if err := yaml.Unmarshal(taskfileData, &root); err != nil {
		log.Fatalf("Error parsing Taskfile: %v", err)
	}

	// 3. Find the "tasks" map node
	tasksNode := findTasksNode(&root)
	if tasksNode == nil {
		log.Fatal("Could not find 'tasks' block in Taskfile.yml")
	}

	// 4. Extract annotated tasks, grouped by workflow → job
	workflows := make(map[string]Jobs)

	for i := 0; i < len(tasksNode.Content); i += 2 {
		keyNode := tasksNode.Content[i]
		valueNode := tasksNode.Content[i+1]
		taskName := keyNode.Value

		// Gather comments from the task key itself
		comments := []string{
			keyNode.HeadComment,
			keyNode.LineComment,
		}

		// Gather comments from all inner fields (like 'desc', 'cmd', 'vars')
		if valueNode.Kind == yaml.MappingNode {
			for j := 0; j < len(valueNode.Content); j++ {
				comments = append(comments, valueNode.Content[j].HeadComment)
				comments = append(comments, valueNode.Content[j].LineComment)
			}
		}

		// Combine all found comments into one block
		fullCommentBlock := strings.Join(comments, "\n")

		if strings.Contains(fullCommentBlock, "@ci:") {
			// Parse "<tag> [step name]" from the comment line.
			parts := strings.SplitN(fullCommentBlock, "@ci:", 2)
			tagLine := strings.SplitN(parts[1], "\n", 2)[0]
			tag, stepName := parseAnnotation(tagLine)

			spec, ok := cfg.Tags[tag]
			if !ok {
				fmt.Fprintf(os.Stderr, "⚠️  Task %q references unknown tag %q (not defined in %s); skipping.\n", taskName, tag, ConfigFile)
				continue
			}

			jobs, ok := workflows[spec.Workflow]
			if !ok {
				jobs = Jobs{}
				workflows[spec.Workflow] = jobs
			}

			// Initialize the job if it doesn't exist
			job, exists := jobs[spec.Job]
			if !exists {
				steps := []Step{{Uses: cfg.Actions.Checkout}}
				if task2ciAsTool {
					steps = append(steps,
						Step{Name: "Check generated CI is up to date", Run: "go tool task2ci -check"},
					)
				} else {
					steps = append(steps,
						Step{Name: "Install task2ci", Uses: cfg.Actions.SetupTask2CI},
						Step{Name: "Check generated CI is up to date", Run: "task2ci -check"},
					)
				}
				if !goTaskAsTool {
					steps = append(steps,
						Step{Name: "Install go-task", Uses: cfg.Actions.SetupTask},
					)
				}
				job = Job{RunsOn: spec.RunsOn, Steps: steps}
			}

			// Resolve the step's display name: annotation override > task desc > task name.
			name := stepName
			if name == "" {
				name = taskDesc(valueNode)
			}
			if name == "" {
				name = taskName
			}
			job.Steps = append(job.Steps, Step{
				Name: name,
				Run:  fmt.Sprintf("%s %s", taskCmd, taskName),
			})

			jobs[spec.Job] = job
		}
	}

	// 5. Render each workflow into a (path → bytes) map for deterministic output.
	wfNames := make([]string, 0, len(workflows))
	for name := range workflows {
		wfNames = append(wfNames, name)
	}
	sort.Strings(wfNames)

	rendered := make(map[string][]byte, len(wfNames))
	for _, name := range wfNames {
		workflow := Workflow{
			Name: name,
			On:   []string{"push", "pull_request"},
			Jobs: workflows[name],
		}
		var buf bytes.Buffer
		buf.WriteString("# THIS FILE IS AUTOGENERATED FROM Taskfile.yml. DO NOT EDIT.\n")
		buf.WriteString("---\n")
		encoder := yaml.NewEncoder(&buf)
		encoder.SetIndent(2)
		if err := encoder.Encode(&workflow); err != nil {
			log.Fatalf("Error encoding workflow %q: %v", name, err)
		}
		rendered[outputPath(name)] = stripTrailingWhitespace(buf.Bytes())
	}

	// 6. Handle -check flag or file write
	if *checkPtr {
		drifted := []string{}
		for _, name := range wfNames {
			path := outputPath(name)
			existing, err := os.ReadFile(path)
			if err != nil {
				log.Fatalf("Check failed: Could not read existing workflow at %s. Have you generated it yet?", path)
			}
			if !bytes.Equal(existing, rendered[path]) {
				drifted = append(drifted, path)
			}
		}
		if len(drifted) > 0 {
			log.Fatalf("❌ ERROR: CI configuration drift detected in: %s. Run the generator locally and commit the updates.", strings.Join(drifted, ", "))
		}
		fmt.Println("✅ CI configuration is up to date.")
		os.Exit(0)
	}

	if err := os.MkdirAll(OutputDir, 0755); err != nil {
		log.Fatalf("Error creating %s: %v", OutputDir, err)
	}
	for _, name := range wfNames {
		path := outputPath(name)
		if err := os.WriteFile(path, rendered[path], 0644); err != nil {
			log.Fatalf("Error writing %s: %v", path, err)
		}
		fmt.Printf("✅ Successfully generated %s\n", path)
	}
}

// parseAnnotation splits an "@ci:" comment payload into (tag, optional step name).
// Syntax: "tag" or "tag | step name". The pipe is the delimiter; whitespace
// around either side is trimmed. Returns ("", "") for an empty line.
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

// findTasksNode traverses the AST to find the "tasks" map
func findTasksNode(root *yaml.Node) *yaml.Node {
	if root.Kind == yaml.DocumentNode {
		root = root.Content[0]
	}
	if root.Kind == yaml.MappingNode {
		for i := 0; i < len(root.Content); i += 2 {
			if root.Content[i].Value == "tasks" {
				return root.Content[i+1]
			}
		}
	}
	return nil
}
