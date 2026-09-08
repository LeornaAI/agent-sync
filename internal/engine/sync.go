package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/bianoble/agent-sync/internal/cache"
	"github.com/bianoble/agent-sync/internal/config"
	"github.com/bianoble/agent-sync/internal/lock"
	"github.com/bianoble/agent-sync/internal/sandbox"
	"github.com/bianoble/agent-sync/internal/source"
	"github.com/bianoble/agent-sync/internal/target"
	"github.com/bianoble/agent-sync/internal/transform"
)

// SyncEngine orchestrates the sync operation.
type SyncEngine struct {
	Registry    *source.Registry
	Cache       *cache.Cache
	ToolMap     *target.ToolMap
	ProjectRoot string
}

// SyncOptions configures a sync operation.
type SyncOptions struct {
	DryRun bool
}

type fileOp struct {
	destPath string
	source   string
	content  []byte
}

// Sync synchronizes files to targets using the lockfile as the source of truth.
// It does NOT modify the lockfile.
func (e *SyncEngine) Sync(ctx context.Context, lf lock.Lockfile, cfg config.Config, opts SyncOptions) (*SyncResult, error) {
	result := &SyncResult{}

	// Resolve all targets.
	targetMap, err := resolveAllTargets(e.ToolMap, cfg)
	if err != nil {
		return nil, fmt.Errorf("resolving targets: %w", err)
	}

	// Snapshot existing target files for rollback.
	var snapshots []snapshot
	var writtenPaths []string

	ops, sourceErrors, err := e.prepareOperations(ctx, lf, cfg, targetMap)
	result.Errors = append(result.Errors, sourceErrors...)
	if err != nil {
		return nil, err
	}

	// Sort ops for deterministic output.
	sort.Slice(ops, func(i, j int) bool {
		return ops[i].destPath < ops[j].destPath
	})

	if opts.DryRun {
		for _, op := range ops {
			absPath, pathErr := sandbox.ValidatePath(e.ProjectRoot, op.destPath)
			if pathErr != nil {
				result.Errors = append(result.Errors, SourceError{Source: op.source, Err: fmt.Errorf("checking %s: %w", op.destPath, pathErr)})
				continue
			}
			existing, err := os.ReadFile(absPath)
			if err != nil {
				result.Written = append(result.Written, FileAction{Path: op.destPath, Action: "new"})
			} else if hex.EncodeToString(sha256Hash(existing)) != hex.EncodeToString(sha256Hash(op.content)) {
				result.Written = append(result.Written, FileAction{Path: op.destPath, Action: "modified"})
			} else {
				result.Skipped = append(result.Skipped, FileAction{Path: op.destPath, Action: "unchanged"})
			}
		}
		return result, nil
	}

	// Snapshot existing files for rollback.
	for _, op := range ops {
		absPath, pathErr := sandbox.ValidatePath(e.ProjectRoot, op.destPath)
		if pathErr != nil {
			result.Errors = append(result.Errors, SourceError{Source: op.source, Err: fmt.Errorf("snapshotting %s: %w", op.destPath, pathErr)})
			continue
		}
		existing, err := os.ReadFile(absPath)
		if err == nil {
			snapshots = append(snapshots, snapshot{path: op.destPath, content: existing, existed: true})
		} else {
			snapshots = append(snapshots, snapshot{path: op.destPath, existed: false})
		}
	}

	// Write files.
	for _, op := range ops {
		absPath, pathErr := sandbox.ValidatePath(e.ProjectRoot, op.destPath)
		if pathErr != nil {
			rollback(e.ProjectRoot, writtenPaths, snapshots)
			result.Errors = append(result.Errors, SourceError{Source: op.source, Err: fmt.Errorf("writing %s: %w", op.destPath, pathErr)})
			return result, fmt.Errorf("sync failed, rolled back: %w", pathErr)
		}
		existing, readErr := os.ReadFile(absPath)
		if readErr == nil && hex.EncodeToString(sha256Hash(existing)) == hex.EncodeToString(sha256Hash(op.content)) {
			result.Skipped = append(result.Skipped, FileAction{Path: op.destPath, Action: "unchanged"})
			continue
		}

		if err := sandbox.SafeWrite(e.ProjectRoot, op.destPath, op.content, 0644); err != nil {
			// Rollback.
			rollback(e.ProjectRoot, writtenPaths, snapshots)
			result.Errors = append(result.Errors, SourceError{Source: op.source, Err: fmt.Errorf("writing %s: %w", op.destPath, err)})
			return result, fmt.Errorf("sync failed, rolled back: %w", err)
		}

		action := "written"
		if readErr == nil {
			action = "modified"
		}
		writtenPaths = append(writtenPaths, op.destPath)
		result.Written = append(result.Written, FileAction{Path: op.destPath, Action: action})
	}

	return result, nil
}

func (e *SyncEngine) prepareOperations(ctx context.Context, lf lock.Lockfile, cfg config.Config, targetMap map[string][]target.ResolvedTarget) ([]fileOp, []SourceError, error) {
	transformsBySource := make(map[string][]config.Transform)
	for _, tx := range cfg.Transforms {
		transformsBySource[tx.Source] = append(transformsBySource[tx.Source], tx)
	}

	var ops []fileOp
	var sourceErrors []SourceError
	for _, ls := range lf.Sources {
		targets, ok := targetMap[ls.Name]
		if !ok {
			continue
		}

		files, err := e.fetchSourceFiles(ctx, ls, cfg)
		if err != nil {
			sourceErrors = append(sourceErrors, SourceError{Source: ls.Name, Err: err})
			continue
		}

		if transforms, ok := transformsBySource[ls.Name]; ok {
			files, err = applyTransforms(files, transforms, cfg.Variables)
			if err != nil {
				sourceErrors = append(sourceErrors, SourceError{Source: ls.Name, Err: err})
				continue
			}
		}

		for _, tgt := range targets {
			for relPath, content := range files {
				ops = append(ops, fileOp{
					destPath: filepath.Join(tgt.Destination, relPath),
					source:   ls.Name,
					content:  content,
				})
			}
		}
	}

	if len(cfg.Overrides) > 0 {
		overrideProc := &transform.OverrideProcessor{ProjectRoot: e.ProjectRoot}
		filesByName := make(map[string][]byte)
		for _, op := range ops {
			filesByName[filepath.Base(op.destPath)] = op.content
		}
		applied, err := overrideProc.Apply(filesByName, cfg.Overrides)
		if err != nil {
			return nil, sourceErrors, fmt.Errorf("applying overrides: %w", err)
		}
		for i, op := range ops {
			if content, ok := applied[filepath.Base(op.destPath)]; ok {
				ops[i].content = content
			}
		}
	}

	return ops, sourceErrors, nil
}

func (e *SyncEngine) fetchSourceFiles(ctx context.Context, ls lock.LockedSource, cfg config.Config) (map[string][]byte, error) {
	files := make(map[string][]byte)

	for relPath, fh := range ls.Resolved.Files {
		// Try cache first.
		if e.Cache != nil {
			content, found, err := e.Cache.Get(fh.SHA256)
			if err != nil {
				return nil, fmt.Errorf("reading %s from cache: %w", relPath, err)
			}
			if found {
				files[relPath] = content
				continue
			}
		}

		// Fetch from source.
		if e.Registry == nil {
			return nil, fmt.Errorf("source registry is required to fetch uncached source %q", ls.Name)
		}
		resolver, err := e.Registry.Get(ls.Type)
		if err != nil {
			return nil, err
		}

		// Build a ResolvedSource from the lockfile entry.
		resolved := &source.ResolvedSource{
			Name:   ls.Name,
			Type:   ls.Type,
			Commit: ls.Resolved.Commit,
			Tree:   ls.Resolved.Tree,
			URL:    ls.Resolved.URL,
			Repo:   ls.Repo,
			Path:   ls.Resolved.Path,
			Files:  make(map[string]string),
		}
		for fp, hash := range ls.Resolved.Files {
			resolved.Files[fp] = hash.SHA256
		}

		fetched, fetchErr := fetchResolvedSource(ctx, resolver, resolved, e.ProjectRoot)
		if fetchErr != nil {
			return nil, fetchErr
		}

		for _, f := range fetched {
			files[f.RelPath] = f.Content
			if e.Cache != nil {
				if err := e.Cache.Put(f.SHA256, f.Content); err != nil {
					return nil, fmt.Errorf("caching %s: %w", f.RelPath, err)
				}
			}
		}
		break // All files fetched in one call
	}

	// If we didn't get all files from the batch fetch, fill remaining from cache.
	for relPath, fh := range ls.Resolved.Files {
		if _, ok := files[relPath]; !ok {
			if e.Cache != nil {
				content, found, err := e.Cache.Get(fh.SHA256)
				if err != nil {
					return nil, fmt.Errorf("reading %s from cache: %w", relPath, err)
				}
				if found {
					files[relPath] = content
				}
			}
			if _, found := files[relPath]; !found {
				return nil, fmt.Errorf("locked file %q was not returned by source %q", relPath, ls.Name)
			}
		}
	}

	return files, nil
}

func applyTransforms(files map[string][]byte, transforms []config.Transform, globalVars map[string]string) (map[string][]byte, error) {
	tmpl := &transform.TemplateTransform{}
	result := make(map[string][]byte, len(files))
	for k, v := range files {
		result[k] = v
	}

	for _, tx := range transforms {
		if tx.Type != "template" {
			continue // custom transforms deferred
		}
		vars := transform.MergeVars(globalVars, tx.Vars)
		for relPath, content := range result {
			out, err := tmpl.Apply(content, vars)
			if err != nil {
				return nil, fmt.Errorf("template transform on %s: %w", relPath, err)
			}
			result[relPath] = out
		}
	}

	return result, nil
}

func rollback(projectRoot string, writtenPaths []string, snapshots []snapshot) {
	snapshotMap := make(map[string]snapshot)
	for _, s := range snapshots {
		snapshotMap[s.path] = s
	}

	for _, path := range writtenPaths {
		s, ok := snapshotMap[path]
		if !ok {
			continue
		}
		if s.existed {
			_ = sandbox.SafeWrite(projectRoot, path, s.content, 0644)
		} else {
			_ = sandbox.SafeRemove(projectRoot, path)
		}
	}
}

type snapshot struct {
	path    string
	content []byte
	existed bool
}

func resolveAllTargets(tm *target.ToolMap, cfg config.Config) (map[string][]target.ResolvedTarget, error) {
	result := make(map[string][]target.ResolvedTarget)
	for _, tgt := range cfg.Targets {
		resolved, err := tm.ResolveTarget(tgt)
		if err != nil {
			return nil, err
		}
		result[tgt.Source] = append(result[tgt.Source], resolved...)
	}
	return result, nil
}

func sha256Hash(data []byte) []byte {
	h := sha256.Sum256(data)
	return h[:]
}
