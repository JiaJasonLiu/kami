// Package workspace implements SYSTEM LAYER 1 of the sandbox architecture:
// code-level directory scoping. A SafeWorkspace confines every file
// operation to a single root directory and rejects any path — however it is
// spelled — that would resolve outside that root, defeating directory
// traversal attacks such as "../../etc/passwd".
//
// This package is the single enforcement point for filesystem access in the
// agent: every tool that touches disk on the model's behalf must go through
// a SafeWorkspace rather than calling the os package directly.
package workspace

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SafeWorkspace is a strict filesystem sandbox rooted at RootPath.
// All user-supplied paths are resolved relative to RootPath and validated
// before any read, write, list, or delete is performed.
type SafeWorkspace struct {
	// RootPath is the absolute path of the sandbox root. Nothing outside
	// this directory can ever be touched through a SafeWorkspace.
	RootPath string
}

// NewSafeWorkspace resolves dirName to an absolute path and returns a
// sandbox rooted there, creating the root directory (mode 0755) if it does
// not already exist. os.MkdirAll is idempotent, so calling this repeatedly
// for the same directory is safe.
func NewSafeWorkspace(dirName string) (*SafeWorkspace, error) {
	if strings.TrimSpace(dirName) == "" {
		return nil, errors.New("workspace: root directory name must not be empty")
	}
	abs, err := filepath.Abs(dirName)
	if err != nil {
		return nil, fmt.Errorf("workspace: cannot resolve %q to an absolute path: %w", dirName, err)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, fmt.Errorf("workspace: cannot create root %q: %w", abs, err)
	}
	return &SafeWorkspace{RootPath: abs}, nil
}

// resolve merges userPath into the workspace root and validates that the
// result cannot escape it. It returns the vetted absolute path.
//
// The defence works in two steps:
//  1. filepath.Clean(filepath.Join(root, userPath)) normalises the merged
//     path, collapsing any "." and ".." segments.
//  2. A strings.HasPrefix check confirms the normalised path is the root
//     itself or lies strictly beneath it. The prefix includes a trailing
//     path separator so a sibling such as "/opt/agent-evil" can never
//     satisfy a root of "/opt/agent".
func (w *SafeWorkspace) resolve(userPath string) (string, error) {
	if strings.TrimSpace(userPath) == "" {
		return "", errors.New("workspace: path must not be empty")
	}
	fullPath := filepath.Clean(filepath.Join(w.RootPath, userPath))
	if fullPath != w.RootPath && !strings.HasPrefix(fullPath, w.RootPath+string(os.PathSeparator)) {
		return "", fmt.Errorf("workspace: security violation: path %q escapes the workspace root", userPath)
	}
	return fullPath, nil
}

// WriteFile writes data to userPath inside the sandbox with mode 0644,
// creating any intermediate directories (mode 0755) so nested paths like
// "notes/projects/ideas.md" work in a single call. It refuses, with a clear
// security error, any path that resolves outside the workspace root.
func (w *SafeWorkspace) WriteFile(userPath string, data []byte) error {
	fullPath, err := w.resolve(userPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		return fmt.Errorf("workspace: cannot create parent directories for %q: %w", userPath, err)
	}
	if err := os.WriteFile(fullPath, data, 0o644); err != nil {
		return fmt.Errorf("workspace: cannot write %q: %w", userPath, err)
	}
	return nil
}

// ReadFile returns the contents of userPath inside the sandbox, applying
// the same path validation as WriteFile.
func (w *SafeWorkspace) ReadFile(userPath string) ([]byte, error) {
	fullPath, err := w.resolve(userPath)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, fmt.Errorf("workspace: cannot read %q: %w", userPath, err)
	}
	return data, nil
}

// DeleteFile removes a single file at userPath inside the sandbox, applying
// the same path validation as WriteFile. It refuses to remove directories.
func (w *SafeWorkspace) DeleteFile(userPath string) error {
	fullPath, err := w.resolve(userPath)
	if err != nil {
		return err
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("workspace: cannot delete %q: %w", userPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("workspace: refusing to delete %q: it is a directory", userPath)
	}
	if err := os.Remove(fullPath); err != nil {
		return fmt.Errorf("workspace: cannot delete %q: %w", userPath, err)
	}
	return nil
}

// ListFiles walks subDir inside the sandbox and returns the workspace-
// relative paths of every regular file beneath it, sorted by filepath.WalkDir
// order (lexical). Pass "" or "." to list the whole workspace. The subDir
// argument goes through the same path validation as every other method.
func (w *SafeWorkspace) ListFiles(subDir string) ([]string, error) {
	if subDir == "" {
		subDir = "."
	}
	dirPath, err := w.resolve(subDir)
	if err != nil {
		return nil, err
	}
	var files []string
	err = filepath.WalkDir(dirPath, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(w.RootPath, p)
		if err != nil {
			return err
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("workspace: cannot list %q: %w", subDir, err)
	}
	return files, nil
}
