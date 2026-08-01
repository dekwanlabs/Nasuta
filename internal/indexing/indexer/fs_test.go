package indexer

import "testing"

func mustDiscoverScanDirs(t *testing.T, root string) []string {
	t.Helper()
	dirs, err := DiscoverScanDirs(root)
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func TestDiscoverScanDirsReportsMissingRepositoriesDirectory(t *testing.T) {
	if _, err := DiscoverScanDirs(t.TempDir()); err == nil {
		t.Fatal("DiscoverScanDirs accepted a missing repos directory")
	}
}
