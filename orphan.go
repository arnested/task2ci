package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

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
