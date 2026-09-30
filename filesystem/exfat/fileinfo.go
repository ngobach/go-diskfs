package exfat

import (
	"io/fs"
	"os"
	"time"
)

// FileInfo represents the metadata for a file or directory in an exFAT filesystem.
// It implements both fs.FileInfo and fs.DirEntry.
type FileInfo struct {
	name    string
	size    int64
	mode    os.FileMode
	modTime time.Time
	isDir   bool
}

var _ fs.FileInfo = (*FileInfo)(nil)
var _ fs.DirEntry = (*FileInfo)(nil)

func (fi *FileInfo) Name() string {
	return fi.name
}

func (fi *FileInfo) Size() int64 {
	return fi.size
}

func (fi *FileInfo) Mode() os.FileMode {
	return fi.mode
}

func (fi *FileInfo) ModTime() time.Time {
	return fi.modTime
}

func (fi *FileInfo) IsDir() bool {
	return fi.isDir
}

func (fi *FileInfo) Sys() any {
	return nil
}

// Type returns the type bits of the FileMode (for fs.DirEntry).
func (fi *FileInfo) Type() fs.FileMode {
	return fi.mode.Type()
}

// Info returns the FileInfo itself (for fs.DirEntry).
func (fi *FileInfo) Info() (fs.FileInfo, error) {
	return fi, nil
}
