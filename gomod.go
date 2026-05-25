package main

import (
	"os"
	"strings"
)

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
