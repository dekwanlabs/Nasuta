package indexing

import (
	"fmt"

	"github.com/dekwanlabs/nasuta/internal/indexing/indexer"
)

func (svc *Service) LoadScanDirs() ([]string, error) {
	if len(svc.Cfg.ScanDirs) > 0 {
		return svc.Cfg.ScanDirs, nil
	}
	return svc.DiscoverScanDirs()
}

// DiscoverScanDirs applies VCS exclusions to workspace scan roots.
func (svc *Service) DiscoverScanDirs() ([]string, error) {
	dirs, err := indexer.DiscoverScanDirs(svc.Cfg.WorkspaceRoot)
	if err != nil {
		return nil, err
	}
	if len(svc.Platform.VCSExcludeProjects) == 0 {
		return dirs, nil
	}
	out := dirs[:0]
	for _, dir := range dirs {
		if !indexer.IsExcluded(dir, svc.Platform.VCSExcludeProjects) {
			out = append(out, dir)
		}
	}
	return out, nil
}

func (svc *Service) refreshScanDirs() error {
	dirs, err := svc.DiscoverScanDirs()
	if err != nil {
		return fmt.Errorf("discover scan directories: %w", err)
	}
	svc.ScanDirs = dirs
	return nil
}
