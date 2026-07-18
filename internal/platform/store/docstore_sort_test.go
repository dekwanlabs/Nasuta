package store

import "testing"

func TestDocListOrderBy(t *testing.T) {
	tests := []struct {
		name      string
		sortBy    string
		sortOrder string
		want      string
	}{
		{
			name: "default newest first",
			want: "ORDER BY updated_at DESC, id DESC",
		},
		{
			name:      "time asc",
			sortBy:    "updated_at",
			sortOrder: "asc",
			want:      "ORDER BY updated_at ASC, id DESC",
		},
		{
			name:      "chunk count desc",
			sortBy:    "chunk_count",
			sortOrder: "desc",
			want:      "ORDER BY chunk_count DESC, updated_at DESC, id DESC",
		},
		{
			name:      "title asc",
			sortBy:    "title",
			sortOrder: "asc",
			want:      "ORDER BY LOWER(title) ASC, updated_at DESC, id DESC",
		},
		{
			name:      "filename desc",
			sortBy:    "filename",
			sortOrder: "desc",
			want:      "ORDER BY LOWER(filename) DESC, updated_at DESC, id DESC",
		},
		{
			name:      "module asc",
			sortBy:    "module",
			sortOrder: "asc",
			want:      "ORDER BY CASE WHEN kind = 'module' THEN 0 ELSE 1 END ASC, LOWER(CASE WHEN kind = 'module' THEN filename ELSE title END) ASC, updated_at DESC, id DESC",
		},
		{
			name:      "invalid sort falls back to time desc",
			sortBy:    "drop table documents",
			sortOrder: "wat",
			want:      "ORDER BY updated_at DESC, id DESC",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := docListOrderBy(tc.sortBy, tc.sortOrder)
			if got != tc.want {
				t.Fatalf("docListOrderBy(%q, %q) = %q, want %q", tc.sortBy, tc.sortOrder, got, tc.want)
			}
		})
	}
}
