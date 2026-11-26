package service

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

var (
	// ErrStorageObjectNotFound indicates the target file does not exist on disk.
	ErrStorageObjectNotFound = errors.New("storage object not found")
	// ErrStorageInvalidPath indicates an unsafe or escaping relative storage key.
	ErrStorageInvalidPath = errors.New("invalid storage key")
)

// DiskStorage manages temporary staging files and atomic promotion into the configured upload root.
type DiskStorage struct {
	rootDir string
	tempDir string
}

// NewDiskStorage initializes a DiskStorage rooted at rootDir with staging files in tempDir
// (defaulting to <rootDir>/.tmp so atomic os.Rename stays on the same filesystem).
func NewDiskStorage(rootDir, tempDir string) (*DiskStorage, error) {
	trimmedRoot := strings.TrimSpace(rootDir)
	if trimmedRoot == "" {
		return nil, errors.New("upload root directory must not be empty")
	}
	absRoot, err := filepath.Abs(filepath.Clean(trimmedRoot))
	if err != nil {
		return nil, fmt.Errorf("resolve upload root: %w", err)
	}

	trimmedTemp := strings.TrimSpace(tempDir)
	if trimmedTemp == "" {
		trimmedTemp = filepath.Join(absRoot, ".tmp")
	}
	absTemp, err := filepath.Abs(filepath.Clean(trimmedTemp))
	if err != nil {
		return nil, fmt.Errorf("resolve upload temp dir: %w", err)
	}

	if err := os.MkdirAll(absRoot, 0o750); err != nil {
		return nil, fmt.Errorf("create upload root dir: %w", err)
	}
	if err := os.MkdirAll(absTemp, 0o750); err != nil {
		return nil, fmt.Errorf("create upload temp dir: %w", err)
	}

	return &DiskStorage{
		rootDir: absRoot,
		tempDir: absTemp,
	}, nil
}

// CreateTemp creates a private temporary file in the configured staging directory.
func (s *DiskStorage) CreateTemp(prefix string) (*os.File, string, error) {
	if s == nil {
		return nil, "", errors.New("disk storage is nil")
	}
	cleanPrefix := strings.NewReplacer("/", "_", "\\", "_", "..", "_").Replace(strings.TrimSpace(prefix))
	if cleanPrefix == "" {
		cleanPrefix = "upload"
	}
	f, err := os.CreateTemp(s.tempDir, cleanPrefix+"-*")
	if err != nil {
		return nil, "", err
	}
	return f, f.Name(), nil
}

// ResolveSafeKey validates that relKey is a clean relative storage key strictly contained inside rootDir.
func (s *DiskStorage) ResolveSafeKey(relKey string) (string, error) {
	if s == nil {
		return "", ErrStorageInvalidPath
	}
	if relKey == "" || strings.ContainsRune(relKey, 0) || strings.ContainsRune(relKey, '\\') {
		return "", ErrStorageInvalidPath
	}
	if strings.HasPrefix(relKey, "/") {
		return "", ErrStorageInvalidPath
	}
	for _, seg := range strings.Split(relKey, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", ErrStorageInvalidPath
		}
	}
	cleaned := path.Clean(relKey)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", ErrStorageInvalidPath
	}

	joined := filepath.Join(s.rootDir, filepath.FromSlash(cleaned))
	rel, err := filepath.Rel(s.rootDir, joined)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrStorageInvalidPath
	}
	return joined, nil
}

// PromoteTempFile atomically moves tempPath to the validated relative key under rootDir.
func (s *DiskStorage) PromoteTempFile(tempPath, relKey string) error {
	destPath, err := s.ResolveSafeKey(relKey)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o750); err != nil {
		return err
	}
	if err := os.Rename(tempPath, destPath); err == nil {
		return nil
	} else if !errors.Is(err, syscall.EXDEV) {
		return err
	}

	// Cross-device fallback: copy into a sibling temp file in the destination directory, fsync, and rename atomically.
	src, err := os.Open(tempPath)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()

	dstTmp, err := os.CreateTemp(filepath.Dir(destPath), ".promote-*")
	if err != nil {
		return err
	}
	dstTmpPath := dstTmp.Name()
	cleanupTmp := true
	defer func() {
		if cleanupTmp {
			_ = dstTmp.Close()
			_ = os.Remove(dstTmpPath)
		}
	}()

	if _, err := io.Copy(dstTmp, src); err != nil {
		return err
	}
	if err := dstTmp.Sync(); err != nil {
		return err
	}
	if err := dstTmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(dstTmpPath, destPath); err != nil {
		return err
	}
	cleanupTmp = false
	_ = os.Remove(tempPath)
	return nil
}

// OpenForRead safely opens an existing regular file by its relative storage key.
func (s *DiskStorage) OpenForRead(relKey string) (*os.File, os.FileInfo, error) {
	fullPath, err := s.ResolveSafeKey(relKey)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrStorageObjectNotFound
		}
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || info.IsDir() || !info.Mode().IsRegular() {
		return nil, nil, ErrStorageInvalidPath
	}
	f, err := os.Open(fullPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil, ErrStorageObjectNotFound
		}
		return nil, nil, err
	}
	return f, info, nil
}

// DeleteQuiet removes a file identified by relKey if present, ignoring missing files.
func (s *DiskStorage) DeleteQuiet(relKey string) error {
	fullPath, err := s.ResolveSafeKey(relKey)
	if err != nil {
		return err
	}
	err = os.Remove(fullPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
