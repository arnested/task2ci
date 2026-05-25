package main

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
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
