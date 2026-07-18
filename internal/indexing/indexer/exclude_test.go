package indexer

import "testing"

func TestIsExcluded(t *testing.T) {
	patterns := []string{"iot/cloud/hsmf/test", "*sentinel-dashboard", "growave"}

	excluded := []string{
		"iot/cloud/hsmf/test",                           // exact path
		"iot/cloud/hsmf/hsmf-mobile-sentinel-dashboard", // glob against project name
		"iot/cloud/hsds/hsds-growave",                   // substring "growave"
	}
	for _, id := range excluded {
		if !IsExcluded(id, patterns) {
			t.Errorf("expected excluded: %s", id)
		}
	}

	keep := []string{
		"iot/cloud/hsas/hsas-app-user",
		"iot/cloud/hsmf/hsmf-mobile-gateway",
	}
	for _, id := range keep {
		if IsExcluded(id, patterns) {
			t.Errorf("should keep: %s", id)
		}
	}

	// a "/"-path pattern must match the flattened "__" dir name and vice-versa
	if !IsExcluded("iot__cloud__hsmf__test", []string{"iot/cloud/hsmf/test"}) {
		t.Error("/-pattern should match flattened dir name (separator normalization)")
	}
	if !IsExcluded("iot/cloud/hsmf/test", []string{"iot__cloud__hsmf__test"}) {
		t.Error("__-pattern should match /-path")
	}
	// generic full-path pattern must NOT over-match a different group
	if IsExcluded("iot/cloud/hsds/hsds-user", []string{"iot/cloud/hsmf/user"}) {
		t.Error("full-path pattern must not match a different group's project")
	}
	// empty patterns never exclude
	if IsExcluded("anything", nil) {
		t.Error("empty patterns must not exclude")
	}
}

func TestFilterProjects(t *testing.T) {
	in := []Project{
		{PathWithNamespace: "iot/cloud/hsas/hsas-app-user"},
		{PathWithNamespace: "iot/cloud/hsmf/test"},
		{PathWithNamespace: "iot/cloud/hsmf/user"},
	}
	kept, excluded := FilterProjects(in, []string{"hsmf/test"})
	if len(kept) != 2 || len(excluded) != 1 {
		t.Fatalf("kept=%d excluded=%d", len(kept), len(excluded))
	}
	if excluded[0].PathWithNamespace != "iot/cloud/hsmf/test" {
		t.Errorf("wrong excluded: %s", excluded[0].PathWithNamespace)
	}
}
