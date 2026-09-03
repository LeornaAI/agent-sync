package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bianoble/agent-sync/internal/config"
)

// LocalResolver resolves and fetches files from the local filesystem.
type LocalResolver struct{}

func (l *LocalResolver) Resolve(ctx context.Context, src config.Source, projectRoot string) (*ResolvedSource, error) {
	if src.Path == "" {
		return nil, &SourceError{Source: src.Name, Operation: "resolve", Err: fmt.Errorf("path is required")}
	}

	absPath := filepath.Join(projectRoot, src.Path)
	absPath = filepath.Clean(absPath)

	// Validate the path is within the project root.
	realPath, err := confinedLocalPath(projectRoot, absPath)
	if err != nil {
		return nil, &SourceError{
			Source:    src.Name,
			Operation: "resolve",
			Err:       fmt.Errorf("path '%s': %w", src.Path, err),
		}
	}
	absPath = realPath

	info, err := os.Stat(absPath)
	if err != nil {
		return nil, &SourceError{Source: src.Name, Operation: "resolve", Err: fmt.Errorf("stat %s: %w", src.Path, err), Hint: "check that the path exists"}
	}

	files := make(map[string]string)

	if !info.IsDir() {
		// Single file.
		hash, hashErr := hashLocalFile(absPath)
		if hashErr != nil {
			return nil, &SourceError{Source: src.Name, Operation: "resolve", Err: hashErr}
		}
		relPath := filepath.Base(absPath)
		files[relPath] = hash
	} else {
		// Directory: walk and hash all files.
		err = filepath.Walk(absPath, func(path string, fi os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if fi.IsDir() {
				return nil
			}
			// Skip hidden files.
			if strings.HasPrefix(fi.Name(), ".") {
				return nil
			}

			confinedPath, confineErr := confinedLocalPath(projectRoot, path)
			if confineErr != nil {
				return confineErr
			}
			rel, relErr := filepath.Rel(absPath, path)
			if relErr != nil {
				return relErr
			}
			hash, hashErr := hashLocalFile(confinedPath)
			if hashErr != nil {
				return hashErr
			}
			files[rel] = hash
			return nil
		})
		if err != nil {
			return nil, &SourceError{Source: src.Name, Operation: "resolve", Err: fmt.Errorf("walking %s: %w", src.Path, err)}
		}
	}

	if len(files) == 0 {
		return nil, &SourceError{
			Source:    src.Name,
			Operation: "resolve",
			Err:       fmt.Errorf("no files found at '%s'", src.Path),
			Hint:      "the path exists but contains no files",
		}
	}

	return &ResolvedSource{
		Name:  src.Name,
		Type:  "local",
		Path:  src.Path,
		Files: files,
	}, nil
}

func (l *LocalResolver) Fetch(ctx context.Context, resolved *ResolvedSource) ([]FetchedFile, error) {
	if resolved.Path == "" {
		return nil, &SourceError{Source: resolved.Name, Operation: "fetch", Err: fmt.Errorf("resolved source missing path")}
	}

	// The path in the resolved source is relative to project root.
	// For local sources during fetch, we need the project root context.
	// We'll look up files relative to the resolved path.
	// Note: the caller should provide the absolute base path. For now,
	// we use the Path as-is since it was resolved during Resolve().
	// The engine layer will handle the project root prefix.

	relPaths := sortedLocalPaths(resolved.Files)
	var fetched []FetchedFile
	for _, relPath := range relPaths {
		expectedHash := resolved.Files[relPath]
		// During fetch, the engine prepends the project root.
		// Here we return the expected structure.
		fetched = append(fetched, FetchedFile{
			RelPath: relPath,
			SHA256:  expectedHash,
		})
	}

	return fetched, nil
}

// FetchWithRoot fetches local files with the project root for path resolution.
func (l *LocalResolver) FetchWithRoot(ctx context.Context, resolved *ResolvedSource, projectRoot string) ([]FetchedFile, error) {
	if resolved == nil || resolved.Path == "" {
		return nil, &SourceError{Source: resolvedName(resolved), Operation: "fetch", Err: fmt.Errorf("resolved source missing path")}
	}
	basePath, err := confinedLocalPath(projectRoot, filepath.Join(projectRoot, resolved.Path))
	if err != nil {
		return nil, &SourceError{Source: resolved.Name, Operation: "fetch", Err: err}
	}

	info, err := os.Stat(basePath)
	if err != nil {
		return nil, &SourceError{Source: resolved.Name, Operation: "fetch", Err: fmt.Errorf("stat %s: %w", resolved.Path, err)}
	}

	relPaths := sortedLocalPaths(resolved.Files)
	var fetched []FetchedFile
	for _, relPath := range relPaths {
		expectedHash := resolved.Files[relPath]
		if err := ctx.Err(); err != nil {
			return nil, &SourceError{Source: resolved.Name, Operation: "fetch", Err: err}
		}
		var absPath string
		if info.IsDir() {
			absPath, err = confinedLocalPath(projectRoot, filepath.Join(basePath, relPath))
			if err != nil {
				return nil, &SourceError{Source: resolved.Name, Operation: "fetch", Err: err}
			}
		} else {
			absPath = basePath
		}

		content, readErr := os.ReadFile(absPath)
		if readErr != nil {
			return nil, &SourceError{Source: resolved.Name, Operation: "fetch", Err: fmt.Errorf("reading %s: %w", relPath, readErr)}
		}

		actualHash := computeLocalHash(content)
		if actualHash != expectedHash {
			return nil, &SourceError{
				Source:    resolved.Name,
				Operation: "fetch",
				Err:       fmt.Errorf("hash mismatch for %s: expected %s, got %s", relPath, expectedHash, actualHash),
				Hint:      "local file has changed since last update — run 'agent-sync update' to re-lock",
			}
		}

		fetched = append(fetched, FetchedFile{
			RelPath: relPath,
			Content: content,
			SHA256:  actualHash,
		})
	}

	return fetched, nil
}

func sortedLocalPaths(files map[string]string) []string {
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func confinedLocalPath(projectRoot, candidate string) (string, error) {
	realRoot, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolving project root: %w", err)
	}
	realRoot, err = filepath.EvalSymlinks(realRoot)
	if err != nil {
		return "", fmt.Errorf("resolving project root symlinks: %w", err)
	}
	realPath, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("resolving path: %w", err)
	}
	realPath, err = filepath.EvalSymlinks(realPath)
	if err != nil {
		return "", fmt.Errorf("resolving path symlinks: %w", err)
	}
	rootPrefix := realRoot + string(filepath.Separator)
	if realPath != realRoot && !strings.HasPrefix(realPath, rootPrefix) {
		return "", fmt.Errorf("path resolves outside project root")
	}
	return realPath, nil
}

func resolvedName(resolved *ResolvedSource) string {
	if resolved == nil {
		return "local"
	}
	return resolved.Name
}

func hashLocalFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return computeLocalHash(data), nil
}

func computeLocalHash(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
