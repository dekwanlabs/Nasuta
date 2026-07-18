package indexer

import "testing"

func TestIsSparseIndexableFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"OrderService.java", true},
		{"handler.go", true},
		{"routes.ts", true},
		{"service.proto", true},
		{"query.sql", true},
		{"pom.xml", false},
		{"application.yaml", false},
		{"README.md", false},
		{"package.json", false},
	}
	for _, tt := range tests {
		if got := IsSparseIndexableFile(tt.name); got != tt.want {
			t.Errorf("IsSparseIndexableFile(%q) = %v; want %v", tt.name, got, tt.want)
		}
	}
}
