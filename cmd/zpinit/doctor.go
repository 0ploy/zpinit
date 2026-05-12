package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/0ploy/zpinit/internal/doctor"
)

// runDoctor executes the doctor checks against configDir and prints
// a grouped, status-prefixed report. Exit code reflects the worst
// finding: 1 on any FAIL, 2 on WARN-only, 0 otherwise. Operators who
// want strict mode can chain `zpinit --doctor || exit 1`.
func runDoctor(configDir string, quiet bool) int {
	checks := doctor.Run(configDir)
	return printDoctor(os.Stdout, configDir, checks, quiet)
}

// printDoctor is split from runDoctor so unit tests can drive it
// against synthetic check lists without exec'ing the binary.
func printDoctor(w io.Writer, configDir string, checks []doctor.Check, quiet bool) int {
	fmt.Fprintf(w, "zpinit doctor: checking %s\n\n", configDir)

	// Preserve the category order in which checks appeared, but group
	// rows by category for readability.
	var cats []string
	byCat := map[string][]doctor.Check{}
	for _, c := range checks {
		if _, seen := byCat[c.Category]; !seen {
			cats = append(cats, c.Category)
		}
		byCat[c.Category] = append(byCat[c.Category], c)
	}
	sort.SliceStable(cats, func(i, j int) bool {
		return categoryOrder(cats[i]) < categoryOrder(cats[j])
	})

	for _, cat := range cats {
		fmt.Fprintln(w, cat)
		for _, c := range byCat[cat] {
			if quiet && c.Status == doctor.StatusOK {
				continue
			}
			fmt.Fprintf(w, "  %-5s %s — %s\n", c.Status, c.Name, c.Detail)
		}
		fmt.Fprintln(w)
	}

	fails, warns := 0, 0
	for _, c := range checks {
		switch c.Status {
		case doctor.StatusFail:
			fails++
		case doctor.StatusWarn:
			warns++
		}
	}
	fmt.Fprintf(w, "summary: %d fail, %d warning\n", fails, warns)

	switch {
	case fails > 0:
		return 1
	case warns > 0:
		return 2
	}
	return 0
}

// categoryOrder returns a deterministic ordering for known
// categories; unknown ones sort last in the order they were inserted.
func categoryOrder(c string) int {
	switch strings.ToLower(c) {
	case "filesystem":
		return 0
	case "config":
		return 1
	case "runtimes":
		return 2
	case "state":
		return 3
	}
	return 99
}
