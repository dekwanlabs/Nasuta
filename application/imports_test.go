package application

import (
	"os/exec"
	"strings"
	"testing"
)

func TestCorePackagesDoNotImportScenarioCapabilities(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "github.com/dekwanlabs/astris/...")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("go list core packages: %v", err)
	}
	if strings.Contains(string(output), "github.com/dekwanlabs/codeloom-scenario") {
		t.Fatal("core packages compile the scenario")
	}
}
