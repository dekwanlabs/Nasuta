package pipeline

import (
	"errors"
	"testing"

	"github.com/dekwanlabs/nasuta/internal/feature/delivery"
)

func TestNormalizeRequest(t *testing.T) {
	request, err := normalizeRequest(Request{
		FeatureID:       "  feat-1  ",
		ClientRequestID: "  client-1  ",
		Repository:      " team/service/ ",
		BaseRef:         " ",
		Provider:        " OpenAI ",
		Model:           " gpt-5 ",
		NetworkEnabled:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Request{
		FeatureID:       "feat-1",
		ClientRequestID: "client-1",
		Repository:      "team/service",
		BaseRef:         "HEAD",
		Provider:        "openai",
		Model:           "gpt-5",
		NetworkEnabled:  true,
	}
	if request != want {
		t.Fatalf("request = %+v, want %+v", request, want)
	}
}

func TestNormalizeRequestRejectsInvalidInput(t *testing.T) {
	for _, test := range []struct {
		name    string
		request Request
	}{
		{name: "feature", request: Request{ClientRequestID: "client-1", Repository: "team/service", Provider: "openai"}},
		{name: "client request", request: Request{FeatureID: "feat-1", Repository: "team/service", Provider: "openai"}},
		{name: "repository", request: Request{FeatureID: "feat-1", ClientRequestID: "client-1", Repository: "../service", Provider: "openai"}},
		{name: "provider", request: Request{FeatureID: "feat-1", ClientRequestID: "client-1", Repository: "team/service"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := normalizeRequest(test.request); !errors.Is(err, delivery.ErrInvalid) {
				t.Fatalf("error = %v, want ErrInvalid", err)
			}
		})
	}
}
