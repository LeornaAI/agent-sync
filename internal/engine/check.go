package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bianoble/agent-sync/internal/cache"
	"github.com/bianoble/agent-sync/internal/config"
	"github.com/bianoble/agent-sync/internal/lock"
	"github.com/bianoble/agent-sync/internal/sandbox"
	"github.com/bianoble/agent-sync/internal/source"
	"github.com/bianoble/agent-sync/internal/target"
)

// CheckEngine verifies that target files match the lockfile.
type CheckEngine struct {
	Registry    *source.Registry
	Cache       *cache.Cache
	ToolMap     *target.ToolMap
	ProjectRoot string
}

// Check verifies target files against the lockfile.
// Returns Clean=true if everything matches.
func (e *CheckEngine) Check(ctx context.Context, lf lock.Lockfile, cfg config.Config) (*CheckResult, error) {
	result := &CheckResult{Clean: true}

	// Resolve all targets.
	targetMap, err := resolveAllTargets(e.ToolMap, cfg)
	if err != nil {
		return nil, err
	}

	// Build locked source lookup.
	lockedByName := make(map[string]lock.LockedSource)
	for _, ls := range lf.Sources {
		lockedByName[ls.Name] = ls
	}
	for _, configuredSource := range cfg.Sources {
		if _, ok := lockedByName[configuredSource.Name]; !ok {
			result.Errors = append(result.Errors, fmt.Errorf("configured source %q is missing from the lockfile", configuredSource.Name))
			result.Clean = false
		}
	}

	if len(cfg.Transforms) > 0 || len(cfg.Overrides) > 0 {
		syncEngine := &SyncEngine{
			Registry:    e.Registry,
			Cache:       e.Cache,
			ToolMap:     e.ToolMap,
			ProjectRoot: e.ProjectRoot,
		}
		ops, sourceErrors, prepareErr := syncEngine.prepareOperations(ctx, lf, cfg, targetMap)
		for _, sourceErr := range sourceErrors {
			result.Errors = append(result.Errors, sourceErr)
			result.Clean = false
		}
		if prepareErr != nil {
			return nil, prepareErr
		}
		for _, op := range ops {
			e.checkFile(result, op.destPath, sha256Hex(op.content))
		}
		return result, nil
	}

	// For each source with targets, check that files exist and match.
	for sourceName, targets := range targetMap {
		ls, ok := lockedByName[sourceName]
		if !ok {
			continue
		}

		for _, tgt := range targets {
			for relPath, fh := range ls.Resolved.Files {
				destPath := filepath.Join(tgt.Destination, relPath)
				e.checkFile(result, destPath, fh.SHA256)
			}
		}
	}

	return result, nil
}

func (e *CheckEngine) checkFile(result *CheckResult, destPath, expectedHash string) {
	absPath, pathErr := sandbox.ValidatePath(e.ProjectRoot, destPath)
	if pathErr != nil {
		result.Errors = append(result.Errors, fmt.Errorf("checking %s: %w", destPath, pathErr))
		result.Clean = false
		return
	}
	content, err := os.ReadFile(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			result.Missing = append(result.Missing, destPath)
		} else {
			result.Errors = append(result.Errors, fmt.Errorf("reading %s: %w", destPath, err))
		}
		result.Clean = false
		return
	}

	actualHash := sha256Hex(content)
	if actualHash != expectedHash {
		result.Drifted = append(result.Drifted, DriftEntry{
			Path:     destPath,
			Expected: expectedHash,
			Actual:   actualHash,
		})
		result.Clean = false
	}
}

func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
