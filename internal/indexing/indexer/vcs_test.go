package indexer

import "testing"

func TestRepoDirName(t *testing.T) {
	cases := map[string]string{
		"group/proj":                 "group/proj",
		"group/sub/proj":             "sub/proj",
		"/group/proj/":               "group/proj",
		"proj":                       "proj",
		"  group/proj  ":             "group/proj",
		"backend/user/hsas-app-user": "user/hsas-app-user",
	}
	for in, want := range cases {
		if got := RepoDirName(in); got != want {
			t.Errorf("RepoDirName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAuthURLInjectsToken(t *testing.T) {
	s := NewSyncer("s3cr3t", 4)
	got, err := s.authURL("https://gitlab.example.com/group/proj.git")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://oauth2:s3cr3t@gitlab.example.com/group/proj.git"
	if got != want {
		t.Errorf("authURL = %q, want %q", got, want)
	}
}
