package codexdata

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

var codexFileLocks sync.Map

func lockForFilePath(authFilePath string) *sync.Mutex {
	cleaned := canonicalLockPath(authFilePath)
	if actual, ok := codexFileLocks.Load(cleaned); ok {
		return actual.(*sync.Mutex)
	}
	mu := &sync.Mutex{}
	actual, _ := codexFileLocks.LoadOrStore(cleaned, mu)
	return actual.(*sync.Mutex)
}

func canonicalLockPath(authFilePath string) string {
	cleaned := filepath.Clean(strings.TrimSpace(authFilePath))
	if abs, errAbs := filepath.Abs(cleaned); errAbs == nil && strings.TrimSpace(abs) != "" {
		cleaned = filepath.Clean(abs)
	}
	if real, errEval := filepath.EvalSymlinks(cleaned); errEval == nil && strings.TrimSpace(real) != "" {
		cleaned = filepath.Clean(real)
	} else if parent, errParent := filepath.EvalSymlinks(filepath.Dir(cleaned)); errParent == nil && strings.TrimSpace(parent) != "" {
		cleaned = filepath.Clean(filepath.Join(parent, filepath.Base(cleaned)))
	}
	return cleaned
}

// WithFileLock executes fn while holding in-process and inter-process locks for authFilePath.
func WithFileLock(authFilePath string, fn func() error) error {
	mu := lockForFilePath(authFilePath)
	mu.Lock()
	defer mu.Unlock()

	if errDir := os.MkdirAll(filepath.Dir(authFilePath), 0o700); errDir != nil {
		return fmt.Errorf("failed to create credential directory: %w", errDir)
	}
	releaseLockFile, errLock := acquireInterProcessLockFile(authFilePath)
	if errLock != nil {
		return errLock
	}
	defer releaseLockFile()
	return fn()
}
