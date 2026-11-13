package logging

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

const fileBodyBufferSize = 16 << 10

// FileBodySource stores large log sections as ordered temp-file parts.
type FileBodySource struct {
	mu           sync.Mutex
	dir          string
	paths        []string
	partFiles    []*os.File
	appendFile   *os.File
	appendBuffer *bufio.Writer
	writeErr     error
	cleanupErr   error
	cleaned      bool
}

// NewFileBodySourceInDir creates a temp-backed source under baseDir.
func NewFileBodySourceInDir(baseDir string, prefix string) (*FileBodySource, error) {
	prefix = sanitizeTempPrefix(prefix)
	baseDir = strings.TrimSpace(baseDir)
	if baseDir == "" {
		return nil, fmt.Errorf("base directory is required")
	}
	if errMkdir := os.MkdirAll(baseDir, 0755); errMkdir != nil {
		return nil, errMkdir
	}
	dir, errCreate := os.MkdirTemp(baseDir, "request-log-parts-"+prefix+"-*")
	if errCreate != nil {
		return nil, errCreate
	}
	return &FileBodySource{dir: dir}, nil
}

func sanitizeTempPrefix(prefix string) string {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" {
		return "log"
	}
	var builder strings.Builder
	for _, r := range prefix {
		switch {
		case r >= 'a' && r <= 'z':
			builder.WriteRune(r)
		case r >= 'A' && r <= 'Z':
			builder.WriteRune(r)
		case r >= '0' && r <= '9':
			builder.WriteRune(r)
		case r == '-' || r == '_':
			builder.WriteRune(r)
		default:
			builder.WriteByte('-')
		}
	}
	out := strings.Trim(builder.String(), "-_")
	if out == "" {
		return "log"
	}
	return out
}

// CreatePart creates one ordered detail log part. The caller must finish writing
// and close it before observing the source or appending bytes to that part.
// Cleanup also closes outstanding handles, including on cancellation.
func (s *FileBodySource) CreatePart(prefix string) (*os.File, error) {
	if s == nil {
		return nil, fmt.Errorf("file body source is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, errCreate := s.createPartLocked(prefix)
	if errCreate == nil {
		s.partFiles = append(s.partFiles, file)
	}
	return file, errCreate
}

func (s *FileBodySource) createPartLocked(prefix string) (*os.File, error) {
	if s.cleaned {
		return nil, fmt.Errorf("file body source has been cleaned")
	}
	if errClose := s.closeAppendLocked(); errClose != nil {
		return nil, errClose
	}
	prefix = sanitizeTempPrefix(prefix)
	if errMkdir := os.MkdirAll(s.dir, 0755); errMkdir != nil {
		return nil, errMkdir
	}
	file, errCreate := os.CreateTemp(s.dir, prefix+"-*.tmp")
	if errCreate != nil {
		return nil, errCreate
	}
	s.paths = append(s.paths, file.Name())
	return file, nil
}

// AppendPart appends one complete ordered part to the source.
func (s *FileBodySource) AppendPart(data []byte) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil
	}
	if s == nil {
		return fmt.Errorf("file body source is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	file, errCreate := s.createPartLocked("part")
	if errCreate != nil {
		return errCreate
	}
	writeErr := writeLogPart(file, data, false)
	if errClose := file.Close(); errClose != nil {
		if writeErr == nil {
			writeErr = errClose
		}
	}
	s.writeErr = writeErr
	return s.writeErr
}

// AppendBytes appends raw bytes to a single ordered part.
func (s *FileBodySource) AppendBytes(data []byte) error {
	if s == nil {
		return fmt.Errorf("file body source is nil")
	}
	if len(data) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaned {
		return fmt.Errorf("file body source has been cleaned")
	}
	if s.writeErr != nil {
		return s.writeErr
	}

	if s.appendFile == nil {
		var file *os.File
		var errOpen error
		if len(s.paths) == 0 {
			file, errOpen = s.createPartLocked("part")
		} else {
			file, errOpen = os.OpenFile(s.paths[len(s.paths)-1], os.O_WRONLY|os.O_APPEND, 0644)
		}
		if errOpen != nil {
			return errOpen
		}
		s.appendFile = file
		if s.appendBuffer == nil {
			s.appendBuffer = bufio.NewWriterSize(file, fileBodyBufferSize)
		} else {
			s.appendBuffer.Reset(file)
		}
	}

	_, s.writeErr = s.appendBuffer.Write(data)
	return s.writeErr
}

// Flush publishes buffered bytes and reports deferred write errors. The buffer
// belongs to this source only; it never delays forwarding a network stream.
func (s *FileBodySource) Flush() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.flushLocked()
}

func (s *FileBodySource) flushLocked() error {
	if s.appendFile != nil && s.writeErr == nil {
		s.writeErr = s.appendBuffer.Flush()
		if s.writeErr == nil {
			// An open handle can still accept writes after its path is removed.
			// Detect that at observation boundaries rather than on every chunk.
			_, s.writeErr = os.Stat(s.appendFile.Name())
		}
	}
	return s.writeErr
}

func (s *FileBodySource) closeAppendLocked() error {
	_ = s.flushLocked()
	if s.appendFile != nil {
		if errClose := s.appendFile.Close(); errClose != nil && s.writeErr == nil {
			s.writeErr = errClose
		}
		s.appendFile = nil
		s.appendBuffer.Reset(nil)
	}
	return s.writeErr
}

// HasPayload reports whether any detail parts were recorded.
func (s *FileBodySource) HasPayload() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.paths) > 0 && !s.cleaned
}

// Paths flushes pending writes and returns a copy of the ordered part paths.
// Its legacy signature cannot return errors; use Flush or WriteTo to observe
// write failures. Failures remain sticky until Cleanup and are also logged here.
func (s *FileBodySource) Paths() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if errFlush := s.flushLocked(); errFlush != nil {
		log.WithError(errFlush).Warn("failed to flush log part files")
	}
	out := make([]string, len(s.paths))
	copy(out, s.paths)
	return out
}

// WriteTo merges all ordered parts into w.
func (s *FileBodySource) WriteTo(w io.Writer) (int64, error) {
	if s == nil || w == nil {
		return 0, nil
	}
	// Keep cleanup and append operations serialized with the entire observation.
	// A path snapshot alone would allow Cleanup to remove unread parts.
	s.mu.Lock()
	defer s.mu.Unlock()
	if errFlush := s.flushLocked(); errFlush != nil {
		return 0, errFlush
	}
	var totalWritten int64
	wrote := false
	for _, path := range s.paths {
		file, errOpen := os.Open(path)
		if errOpen != nil {
			if os.IsNotExist(errOpen) {
				continue
			}
			return totalWritten, errOpen
		}
		if wrote {
			n, errWrite := io.WriteString(w, "\n")
			totalWritten += int64(n)
			if errWrite != nil {
				if errClose := file.Close(); errClose != nil {
					log.WithError(errClose).Warn("failed to close log part file")
				}
				return totalWritten, errWrite
			}
		}
		n, errCopy := io.Copy(w, file)
		totalWritten += n
		if errClose := file.Close(); errClose != nil {
			log.WithError(errClose).Warn("failed to close log part file")
			if errCopy == nil {
				errCopy = errClose
			}
		}
		if errCopy != nil {
			return totalWritten, errCopy
		}
		wrote = true
	}
	return totalWritten, nil
}

// Bytes merges all ordered parts into memory.
func (s *FileBodySource) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	if _, errWrite := s.WriteTo(&buf); errWrite != nil {
		return nil, errWrite
	}
	return buf.Bytes(), nil
}

// Cleanup removes all temp detail parts and their directory.
func (s *FileBodySource) Cleanup() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cleaned {
		return s.cleanupErr
	}
	s.cleaned = true

	firstErr := s.closeAppendLocked()
	for _, file := range s.partFiles {
		if errClose := file.Close(); errClose != nil && !errors.Is(errClose, os.ErrClosed) && firstErr == nil {
			firstErr = errClose
		}
	}
	s.partFiles = nil
	s.appendBuffer = nil
	for _, path := range s.paths {
		if errRemove := os.Remove(path); errRemove != nil && !os.IsNotExist(errRemove) && firstErr == nil {
			firstErr = errRemove
		}
	}
	if s.dir != "" {
		if errRemove := os.RemoveAll(s.dir); errRemove != nil && firstErr == nil {
			firstErr = errRemove
		}
	}
	s.paths = nil
	s.cleanupErr = firstErr
	return s.cleanupErr
}

func cleanupFileBodySources(sources ...*FileBodySource) {
	for _, source := range sources {
		if source == nil {
			continue
		}
		if errCleanup := source.Cleanup(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up log part files")
		}
	}
}
