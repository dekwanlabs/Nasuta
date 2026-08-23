package investigation

import (
	"encoding/json"
	"fmt"
	"os"
)

// EvaluationCase is a provider-independent regression fixture. It captures the
// minimal contract and delivery expectations needed to keep the empty-answer
// and traceability gates closed.
type EvaluationCase struct {
	ID                 string                `json:"id"`
	Contract           InvestigationContract `json:"contract"`
	ExpectNonEmpty     bool                  `json:"expect_non_empty"`
	ExpectTraceable    bool                  `json:"expect_traceable"`
	ExpectGoalsCovered bool                  `json:"expect_goals_covered"`
}

// EvaluationSuite is the durable set of regression cases. Cases are stored as
// JSON so a deployed environment can replay them independently of the source
// tree, while evaluation remains deterministic and provider-independent.
type EvaluationSuite struct {
	Cases []EvaluationCase `json:"cases"`
}

// SaveEvaluationSuite writes the suite atomically via a temporary file.
func SaveEvaluationSuite(path string, suite EvaluationSuite) error {
	if path == "" {
		return fmt.Errorf("evaluation suite path is required")
	}
	data, err := json.MarshalIndent(suite, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// LoadEvaluationSuite reads the regression fixture from disk.
func LoadEvaluationSuite(path string) (EvaluationSuite, error) {
	if path == "" {
		return EvaluationSuite{}, fmt.Errorf("evaluation suite path is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return EvaluationSuite{}, err
	}
	var suite EvaluationSuite
	if err := json.Unmarshal(data, &suite); err != nil {
		return EvaluationSuite{}, err
	}
	return suite, nil
}

// EvaluateSuite applies EvaluateDelivery to each stored case and returns one
// result per case, preserving failures for the regression report.
func EvaluateSuite(suite EvaluationSuite, runs map[string]InvestigationRun) map[string]EvaluationResult {
	results := make(map[string]EvaluationResult, len(suite.Cases))
	for _, fixture := range suite.Cases {
		run, ok := runs[fixture.ID]
		if !ok {
			results[fixture.ID] = EvaluationResult{RunID: fixture.ID, Failures: []string{"run is missing"}}
			continue
		}
		results[fixture.ID] = EvaluateDelivery(run)
	}
	return results
}
