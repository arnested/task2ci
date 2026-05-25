package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

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
