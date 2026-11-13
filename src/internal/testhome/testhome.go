// Package testhome isolates tests from the developer's home directory.
//
// Key discovery falls back to ~/.switch/prod.keys, and macOS settings live
// under the user config directory. Tests that reached the real home directory
// could load real keys, behave differently per machine, and print key values.
package testhome

import (
	"fmt"
	"os"
	"testing"
)

// Run points HOME, USERPROFILE and XDG_CONFIG_HOME at an empty temporary
// directory for the whole test binary, runs the tests, and removes it.
// Call it from TestMain: os.Exit(testhome.Run(m)).
func Run(m *testing.M) int {
	home, err := os.MkdirTemp("", "slm-test-home-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create isolated test home: %v\n", err)
		return 1
	}
	for _, name := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		if err := os.Setenv(name, home); err != nil {
			fmt.Fprintf(os.Stderr, "set %s for isolated test home: %v\n", name, err)
			return 1
		}
	}
	code := m.Run()
	if err := os.RemoveAll(home); err != nil {
		fmt.Fprintf(os.Stderr, "remove isolated test home %q: %v\n", home, err)
		if code == 0 {
			code = 1
		}
	}
	return code
}
