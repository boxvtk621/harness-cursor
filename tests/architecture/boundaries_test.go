package architecture_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestStandaloneRepositoryBoundaries(t *testing.T) {
	root := repositoryRoot(t)
	for _, rel := range []string{
		"adapters/codex", "agent-service", "panel", "router", "fixik", "go.work",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Errorf("forbidden standalone path exists: %s", rel)
		}
	}

	goMod := readFile(t, filepath.Join(root, "go.mod"))
	for _, forbidden := range []string{" replace ", "\nreplace ", "homelab-telegram-panel", "../"} {
		if strings.Contains(goMod, forbidden) {
			t.Errorf("go.mod contains forbidden dependency %q", forbidden)
		}
	}

	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "node_modules" || entry.Name() == "_bmad" ||
				entry.Name() == "_bmad-output" || entry.Name() == ".provenance-source" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		if strings.Contains(filepath.ToSlash(path), "/tests/architecture/") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			importPath, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if forbiddenImportPath(importPath) {
				t.Errorf("%s imports forbidden component %q", filepath.ToSlash(path), importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestForbiddenImportPath(t *testing.T) {
	for _, importPath := range []string{
		"github.com/boxvtk621/homelab-telegram-panel/harness/runtime",
		"example.com/project/agent-service/client",
		"example.com/project/panel/runtime",
		"example.com/project/router/contracts",
		"example.com/project/fixik/api",
		"example.com/project/adapters/codex",
	} {
		if !forbiddenImportPath(importPath) {
			t.Errorf("forbidden import was accepted: %s", importPath)
		}
	}
	for _, importPath := range []string{
		"github.com/boxvtk621/harness-cursor/runtime",
		"example.com/project/panels/runtime",
		"example.com/project/router-client/contracts",
	} {
		if forbiddenImportPath(importPath) {
			t.Errorf("allowed import was rejected: %s", importPath)
		}
	}
}

func forbiddenImportPath(importPath string) bool {
	const sourceModule = "github.com/boxvtk621/homelab-telegram-panel"
	if importPath == sourceModule || strings.HasPrefix(importPath, sourceModule+"/") {
		return true
	}
	segments := strings.Split(importPath, "/")
	for index, segment := range segments {
		switch segment {
		case "agent-service", "panel", "router", "fixik":
			return true
		case "codex":
			if index > 0 && segments[index-1] == "adapters" {
				return true
			}
		}
	}
	return false
}

func TestCursorImageContextIsDenyFirstAndStateClosed(t *testing.T) {
	root := repositoryRoot(t)
	body := readFile(t, filepath.Join(root, "delivery", "Dockerfile.cursor.dockerignore"))
	lines := strings.Split(body, "\n")
	if len(lines) == 0 || lines[0] != "**" {
		t.Fatal("Cursor image context must start with deny-all")
	}
	for _, required := range []string{
		"**/.env*", "**/*.key", "**/*.pem", "**/*.sqlite", "**/*.db", "**/.git", "**/_bmad-output", "**/node_modules",
	} {
		if !strings.Contains(body, required+"\n") {
			t.Errorf("Cursor image context is missing forbidden pattern %q", required)
		}
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate architecture test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
