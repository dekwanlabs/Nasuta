package semantic

import "testing"

func TestRequiredCapabilitiesReturnsValueCopy(t *testing.T) {
	first := RequiredCapabilities()
	first.Dense = false

	if !RequiredCapabilities().Dense {
		t.Fatal("required capabilities were mutated through a returned value")
	}
}

func TestValidateCapabilitiesRejectsMissingBehavior(t *testing.T) {
	actual := RequiredCapabilities()
	actual.GroupBy = false

	if err := ValidateCapabilities("test", actual); err == nil {
		t.Fatal("missing GroupBy capability unexpectedly accepted")
	}
}
