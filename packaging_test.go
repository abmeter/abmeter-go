package abmeter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The packaging boundary, adapted for Go: the published artifact is the
// mirror repo itself (a tagged module — Go has no registry), so the guards
// run against the source tree that bin/sync-to-public mirrors.

// Zero non-stdlib dependencies is a published guarantee: go.mod must carry
// no require directives.
func TestGoModHasZeroDependencies(t *testing.T) {
	raw, err := os.ReadFile("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "require") {
		t.Errorf("go.mod must have zero dependencies, got:\n%s", raw)
	}
}

// The dependency runs one way — canonical app -> SDK, never the reverse. No
// SDK source may reference the harness.
func TestNoHarnessReferences(t *testing.T) {
	entries, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range entries {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "canonical-apps") && path != "packaging_test.go" {
			t.Errorf("%s references canonical-apps — the SDK must stay self-contained", path)
		}
	}
}

// Required-file guard (the "ships too little" direction): the files the
// mirror must contain for the module to be usable and documented.
func TestRequiredFilesPresent(t *testing.T) {
	required := []string{
		"go.mod", "LICENSE.txt", "README.md",
		"abmeter.go", "version.go", "numutils.go", "assignment_config.go",
		"resolver.go", "resolver_provider.go", "client.go", "submitter.go",
		"config.go", "types.go", "apierror.go", "user.go",
		"testdata/num_utils_parity.json",
	}
	for _, path := range required {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("required file missing: %s", path)
		}
	}
}
