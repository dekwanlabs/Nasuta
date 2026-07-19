package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path"
	"strings"
	"testing"
)

func TestCorePackagesDoNotImportScenarioCapabilities(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "github.com/dekwanlabs/nasuta/...")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list core packages: %v", err)
	}
	if strings.Contains(string(output), "github.com/dekwanlabs/codeloom-scenario") {
		t.Fatal("core packages compile the scenario")
	}
}

func TestPackageNamesMatchDirectories(t *testing.T) {
	command := exec.Command("go", "list", "-json", "github.com/dekwanlabs/nasuta/...")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list core packages: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var pkg struct {
			ImportPath string
			Name       string
		}
		if err := decoder.Decode(&pkg); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode package metadata: %v", err)
		}
		if pkg.Name != "main" && pkg.Name != path.Base(pkg.ImportPath) {
			t.Errorf("package %s declares %q; want directory name %q", pkg.ImportPath, pkg.Name, path.Base(pkg.ImportPath))
		}
	}
}
