package sessionhistory

import "testing"

func TestIsLowInformationSupportsConfiguredLanguages(t *testing.T) {
	tests := map[string][]string{
		"english":  {"the", "how", "now", "need", "can"},
		"chinese":  {"怎么", "这个", "现在", "需要", "可以"},
		"german":   {"der", "für", "dieser", "wann", "können"},
		"italian":  {"gli", "questo", "quando", "dove", "può"},
		"spanish":  {"los", "este", "qué", "cuándo", "puede"},
		"japanese": {"これ", "その", "何", "どこ", "できます"},
		"korean":   {"이것", "그것", "무엇", "어디", "가능"},
	}
	for language, values := range tests {
		for _, value := range values {
			t.Run(language+"/"+value, func(t *testing.T) {
				if !isLowInformation(value) {
					t.Fatalf("isLowInformation(%q) = false, want true", value)
				}
			})
		}
	}
	if isLowInformation("createCart") {
		t.Fatal("technical identifier was classified as low information")
	}
}
