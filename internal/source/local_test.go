package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bianoble/agent-sync/internal/config"
)

func TestLocalResolverDirectory(t *testing.T) {
	root := t.TempDir()
	agentsDir := filepath.Join(root, "agents", "standards")
	if err := os.MkdirAll(agentsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "naming.md"), []byte("# Naming\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "testing.md"), []byte("# Testing\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &LocalResolver{}
	src := config.Source{Name: "standards", Type: "local", Path: "./agents/standards/"}

	resolved, err := r.Resolve(context.Background(), src, root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if resolved.Name != "standards" {
		t.Errorf("name = %q", resolved.Name)
	}
	if resolved.Type != "local" {
		t.Errorf("type = %q", resolved.Type)
	}
	if len(resolved.Files) != 2 {
		t.Errorf("files count = %d, want 2: %v", len(resolved.Files), resolved.Files)
	}
	if _, ok := resolved.Files["naming.md"]; !ok {
		t.Errorf("expected naming.md in files")
	}
	if _, ok := resolved.Files["testing.md"]; !ok {
		t.Errorf("expected testing.md in files")
	}
}

func TestLocalResolverSingleFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "policy.md"), []byte("# Policy\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &LocalResolver{}
	src := config.Source{Name: "policy", Type: "local", Path: "./policy.md"}

	resolved, err := r.Resolve(context.Background(), src, root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if len(resolved.Files) != 1 {
		t.Fatalf("files count = %d, want 1", len(resolved.Files))
	}
	if _, ok := resolved.Files["policy.md"]; !ok {
		t.Errorf("expected policy.md in files")
	}
}

func TestLocalResolverMissingPath(t *testing.T) {
	r := &LocalResolver{}
	_, err := r.Resolve(context.Background(), config.Source{Name: "test", Type: "local"}, t.TempDir())
	if err == nil {
		t.Fatal("expected error for missing path")
	}
	if !strings.Contains(err.Error(), "path is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLocalResolverNonexistentPath(t *testing.T) {
	r := &LocalResolver{}
	src := config.Source{Name: "test", Type: "local", Path: "./does-not-exist/"}
	_, err := r.Resolve(context.Background(), src, t.TempDir())
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestLocalResolverEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0755); err != nil {
		t.Fatal(err)
	}

	r := &LocalResolver{}
	src := config.Source{Name: "empty", Type: "local", Path: "./empty/"}

	_, err := r.Resolve(context.Background(), src, root)
	if err == nil {
		t.Fatal("expected error for empty directory")
	}
	if !strings.Contains(err.Error(), "no files found") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLocalResolverRejectsEscapePath(t *testing.T) {
	root := t.TempDir()
	r := &LocalResolver{}
	src := config.Source{Name: "escape", Type: "local", Path: "../../etc/passwd"}

	_, err := r.Resolve(context.Background(), src, root)
	if err == nil {
		t.Fatal("expected error for path escape")
	}
}

func TestLocalFetchWithRootRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	content := []byte("outside")
	outsideFile := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(outsideFile, content, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "linked.md")); err != nil {
		t.Fatal(err)
	}
	resolved := &ResolvedSource{
		Name: "escape", Type: "local", Path: "linked.md",
		Files: map[string]string{"linked.md": computeLocalHash(content)},
	}

	_, err := (&LocalResolver{}).FetchWithRoot(context.Background(), resolved, root)
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Fatalf("FetchWithRoot() error = %v, want path confinement error", err)
	}
}

func TestLocalResolveRejectsNestedSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	sourceDir := filepath.Join(root, "source")
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

	_, err := (&LocalResolver{}).Resolve(context.Background(), config.Source{
		Name: "escape", Type: "local", Path: "source",
	}, root)
	if err == nil || !strings.Contains(err.Error(), "outside project root") {
		t.Fatalf("Resolve() error = %v, want path confinement error", err)
	}
}

func TestLocalFetchOrderIsDeterministic(t *testing.T) {
	resolved := &ResolvedSource{
		Name: "ordered", Type: "local", Path: "source",
		Files: map[string]string{"z.md": "z", "a.md": "a", "m.md": "m"},
	}
	fetched, err := (&LocalResolver{}).Fetch(context.Background(), resolved)
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []string{"a.md", "m.md", "z.md"} {
		if fetched[index].RelPath != want {
			t.Fatalf("fetched[%d].RelPath = %q, want %q", index, fetched[index].RelPath, want)
		}
	}
}

func TestLocalResolverSkipsHiddenFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "src")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visible.md"), []byte("visible"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("hidden"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &LocalResolver{}
	src := config.Source{Name: "test", Type: "local", Path: "./src/"}

	resolved, err := r.Resolve(context.Background(), src, root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(resolved.Files) != 1 {
		t.Errorf("expected 1 file (hidden should be skipped), got %d: %v", len(resolved.Files), resolved.Files)
	}
}

func TestLocalFetchWithRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "agents")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rules.md"), []byte("# Rules\n"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &LocalResolver{}
	src := config.Source{Name: "test", Type: "local", Path: "./agents/"}

	resolved, err := r.Resolve(context.Background(), src, root)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	fetched, err := r.FetchWithRoot(context.Background(), resolved, root)
	if err != nil {
		t.Fatalf("FetchWithRoot: %v", err)
	}
	if len(fetched) != 1 {
		t.Fatalf("expected 1 file, got %d", len(fetched))
	}
	if string(fetched[0].Content) != "# Rules\n" {
		t.Errorf("content = %q", string(fetched[0].Content))
	}
}

func TestLocalFetchHashMismatch(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "src")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file.md"), []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}

	r := &LocalResolver{}
	resolved := &ResolvedSource{
		Name:  "test",
		Type:  "local",
		Path:  "./src/",
		Files: map[string]string{"file.md": "wrong_hash"},
	}

	_, err := r.FetchWithRoot(context.Background(), resolved, root)
	if err == nil {
		t.Fatal("expected error for hash mismatch")
	}
	if !strings.Contains(err.Error(), "hash mismatch") {
		t.Errorf("unexpected error: %v", err)
	}
}
