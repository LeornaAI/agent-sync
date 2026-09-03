package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/bianoble/agent-sync/internal/cache"
	"github.com/bianoble/agent-sync/internal/config"
	"github.com/bianoble/agent-sync/internal/lock"
	"github.com/bianoble/agent-sync/internal/source"
	"github.com/bianoble/agent-sync/internal/target"
)

func TestLocalSourceUpdateAndSyncPreserveContent(t *testing.T) {
	projectRoot := t.TempDir()
	sourceDir := filepath.Join(projectRoot, "source")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	want := []byte("# Rules\n\x00byte-identical\n")
	if err := os.WriteFile(filepath.Join(sourceDir, "rules.md"), want, 0644); err != nil {
		t.Fatal(err)
	}

	registry := source.NewRegistry()
	registry.Register("local", &source.LocalResolver{})
	contentCache, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Version: 1,
		Sources: []config.Source{{Name: "rules", Type: "local", Path: "source"}},
		Targets: []config.Target{{Source: "rules", Destination: ".out"}},
	}

	updated, err := (&UpdateEngine{Registry: registry, Cache: contentCache, ProjectRoot: projectRoot}).Update(
		context.Background(), cfg, nil, UpdateOptions{},
	)
	if err != nil || len(updated.Failed) != 0 || updated.Lockfile == nil {
		t.Fatalf("Update() = %+v, %v", updated, err)
	}
	fileHash := updated.Lockfile.Sources[0].Resolved.Files["rules.md"].SHA256
	cached, found, err := contentCache.Get(fileHash)
	if err != nil || !found || !bytes.Equal(cached, want) {
		t.Fatalf("cached content = %q, found=%v, err=%v; want %q", cached, found, err, want)
	}

	synced, err := (&SyncEngine{
		Registry: registry, Cache: contentCache, ToolMap: target.NewToolMap(nil), ProjectRoot: projectRoot,
	}).Sync(context.Background(), *updated.Lockfile, cfg, SyncOptions{})
	if err != nil || len(synced.Errors) != 0 {
		t.Fatalf("Sync() = %+v, %v", synced, err)
	}
	got, err := os.ReadFile(filepath.Join(projectRoot, ".out", "rules.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("synced content = %q, want byte-identical %q", got, want)
	}
}

func TestLocalSourceUpdateRejectsNestedSymlinkEscape(t *testing.T) {
	projectRoot := t.TempDir()
	sourceDir := filepath.Join(projectRoot, "source")
	if err := os.MkdirAll(sourceDir, 0755); err != nil {
		t.Fatal(err)
	}
	outsideFile := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outsideFile, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(sourceDir, "linked.md")); err != nil {
		t.Fatal(err)
	}
	registry := source.NewRegistry()
	registry.Register("local", &source.LocalResolver{})
	contentCache, err := cache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Version: 1, Sources: []config.Source{{Name: "escape", Type: "local", Path: "source"}}}

	result, err := (&UpdateEngine{Registry: registry, Cache: contentCache, ProjectRoot: projectRoot}).Update(
		context.Background(), cfg, nil, UpdateOptions{},
	)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if len(result.Failed) != 1 || len(result.Updated) != 0 || len(result.Lockfile.Sources) != 0 {
		t.Fatalf("Update() result = %+v, want one failure and no updated/locked source", result)
	}
}

func TestUpdateEngineUnknownSourceName(t *testing.T) {
	reg := newTestRegistry(map[string]*mockResolver{})
	eng := &UpdateEngine{Registry: reg, ProjectRoot: t.TempDir()}

	cfg := config.Config{
		Version: 1,
		Sources: []config.Source{{Name: "exists", Type: "local", Path: "./a/"}},
	}

	result, err := eng.Update(context.Background(), cfg, nil, UpdateOptions{
		SourceNames: []string{"nonexistent"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(result.Failed) != 1 {
		t.Fatalf("failed = %d, want 1", len(result.Failed))
	}
	if result.Failed[0].Source != "nonexistent" {
		t.Errorf("failed source = %q", result.Failed[0].Source)
	}
}

func TestUpdateEngineResolveError(t *testing.T) {
	reg := newTestRegistry(map[string]*mockResolver{
		"local": {err: fmt.Errorf("resolve failed")},
	})
	eng := &UpdateEngine{Registry: reg, ProjectRoot: t.TempDir()}

	cfg := config.Config{
		Version: 1,
		Sources: []config.Source{{Name: "src", Type: "local", Path: "./a/"}},
	}

	result, err := eng.Update(context.Background(), cfg, nil, UpdateOptions{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(result.Failed) != 1 {
		t.Fatalf("failed = %d, want 1", len(result.Failed))
	}
}

func TestUpdateEngineUnknownType(t *testing.T) {
	reg := newTestRegistry(map[string]*mockResolver{})
	eng := &UpdateEngine{Registry: reg, ProjectRoot: t.TempDir()}

	cfg := config.Config{
		Version: 1,
		Sources: []config.Source{{Name: "src", Type: "unknown-type", Path: "./a/"}},
	}

	result, err := eng.Update(context.Background(), cfg, nil, UpdateOptions{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if len(result.Failed) != 1 {
		t.Fatalf("failed = %d, want 1", len(result.Failed))
	}
}

func TestUpdateEngineWithExistingLock(t *testing.T) {
	contentHash := cache.ComputeHash([]byte("content"))

	reg := newTestRegistry(map[string]*mockResolver{
		"local": {
			resolved: &source.ResolvedSource{
				Name:  "src-a",
				Type:  "local",
				Path:  "./a/",
				Files: map[string]string{"a.md": contentHash},
			},
			files: []source.FetchedFile{
				{RelPath: "a.md", Content: []byte("content"), SHA256: contentHash},
			},
		},
	})

	cacheDir := t.TempDir()
	c, _ := cache.New(cacheDir)

	eng := &UpdateEngine{
		Registry:    reg,
		Cache:       c,
		ProjectRoot: t.TempDir(),
	}

	cfg := config.Config{
		Version: 1,
		Sources: []config.Source{
			{Name: "src-a", Type: "local", Path: "./a/"},
			{Name: "src-b", Type: "local", Path: "./b/"},
		},
	}

	existingLock := &lock.Lockfile{
		Version: 1,
		Sources: []lock.LockedSource{
			{Name: "src-a", Type: "local", Resolved: lock.ResolvedState{Path: "./a/"}, Status: "ok"},
			{Name: "src-b", Type: "local", Resolved: lock.ResolvedState{Path: "./b/"}, Status: "ok"},
		},
	}

	// Only update src-a, src-b should keep its previous state.
	result, err := eng.Update(context.Background(), cfg, existingLock, UpdateOptions{
		SourceNames: []string{"src-a"},
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	if result.Lockfile == nil {
		t.Fatal("lockfile should not be nil")
	}
	if len(result.Lockfile.Sources) != 2 {
		t.Fatalf("lockfile sources = %d, want 2", len(result.Lockfile.Sources))
	}

	// src-a should have new resolved state.
	if result.Updated[0].Before == nil {
		t.Error("before should not be nil for existing source")
	}
}

func TestResolvedToLockedGit(t *testing.T) {
	src := config.Source{Name: "rules", Type: "git", Repo: "https://github.com/org/rules.git"}
	resolved := &source.ResolvedSource{
		Name:   "rules",
		Type:   "git",
		Commit: "abc123def456",
		Tree:   "tree789",
		Files:  map[string]string{"file.md": "hash1"},
	}

	ls := resolvedToLocked(src, resolved)

	if ls.Name != "rules" {
		t.Errorf("name = %q", ls.Name)
	}
	if ls.Type != "git" {
		t.Errorf("type = %q", ls.Type)
	}
	if ls.Resolved.Commit != "abc123def456" {
		t.Errorf("commit = %q", ls.Resolved.Commit)
	}
	if ls.Resolved.Tree != "tree789" {
		t.Errorf("tree = %q", ls.Resolved.Tree)
	}
	if ls.Status != "ok" {
		t.Errorf("status = %q", ls.Status)
	}
	if ls.Resolved.Files["file.md"].SHA256 != "hash1" {
		t.Errorf("file hash = %q", ls.Resolved.Files["file.md"].SHA256)
	}
}

func TestResolvedToLockedURL(t *testing.T) {
	src := config.Source{Name: "policy", Type: "url", URL: "https://example.com/policy.md"}
	resolved := &source.ResolvedSource{
		Name:  "policy",
		Type:  "url",
		URL:   "https://example.com/policy.md",
		Files: map[string]string{"policy.md": "sha256hash"},
	}

	ls := resolvedToLocked(src, resolved)

	if ls.Resolved.SHA256 != "sha256hash" {
		t.Errorf("sha256 = %q, want sha256hash", ls.Resolved.SHA256)
	}
	if ls.Resolved.URL != "https://example.com/policy.md" {
		t.Errorf("url = %q", ls.Resolved.URL)
	}
}

func TestResolvedSHA256URL(t *testing.T) {
	resolved := &source.ResolvedSource{
		Type:  "url",
		Files: map[string]string{"file.md": "the-sha256-hash"},
	}

	got := resolvedSHA256(resolved)
	if got != "the-sha256-hash" {
		t.Errorf("resolvedSHA256 = %q, want the-sha256-hash", got)
	}
}

func TestResolvedSHA256NonURL(t *testing.T) {
	resolved := &source.ResolvedSource{
		Type:  "git",
		Files: map[string]string{"file.md": "hash1"},
	}

	got := resolvedSHA256(resolved)
	if got != "" {
		t.Errorf("resolvedSHA256 for git = %q, want empty", got)
	}
}

func TestResolvedSHA256Empty(t *testing.T) {
	resolved := &source.ResolvedSource{
		Type: "url",
	}

	got := resolvedSHA256(resolved)
	if got != "" {
		t.Errorf("resolvedSHA256 with no files = %q, want empty", got)
	}
}
