package pipeline

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func DiscoverPipelines(rootPath string) ([]*PipelineConfig, error) {
	absRoot, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, fmt.Errorf("resolve root path: %w", err)
	}

	var configs []*PipelineConfig

	err = filepath.WalkDir(absRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if !d.IsDir() {
			return nil
		}

		base := filepath.Base(path)
		if base == ".monofs" || base == "pipelines" {
			return nil
		}

		if strings.HasPrefix(base, ".") && base != ".monofs" {
			return filepath.SkipDir
		}

		pipelineDir := filepath.Join(path, ".monofs", "pipelines")
		if info, err := os.Stat(pipelineDir); err == nil && info.IsDir() {
			entries, err := os.ReadDir(pipelineDir)
			if err != nil {
				return nil
			}
			for _, entry := range entries {
				if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".yml") {
					continue
				}
				fullPath := filepath.Join(pipelineDir, entry.Name())
				cfg, err := LoadConfig(fullPath)
				if err != nil {
					continue
				}

				relDir, _ := filepath.Rel(absRoot, path)
				cfg.SourceDir = relDir

				if cfg.SourceDir != "." && cfg.SourceDir != "" {
					cfg.Name = filepath.Join(cfg.SourceDir, cfg.Name)
				}

				configs = append(configs, cfg)
			}
		}

		return nil
	})

	if err != nil {
		return nil, fmt.Errorf("walk pipeline dirs: %w", err)
	}

	return configs, nil
}
