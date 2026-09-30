package exfat

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"github.com/diskfs/go-diskfs/backend"
	"github.com/diskfs/go-diskfs/filesystem"
)

var _ filesystem.FileSystem = (*FileSystem)(nil)

// FileSystem represents an exFAT filesystem instance.
type FileSystem struct {
	part    *Partition
	rootDir *Directory
}

// Create formats an exFAT filesystem on the provided storage and returns the opened FileSystem.
func Create(b backend.Storage, size, start, blocksize int64, volumeLabel string) (*FileSystem, error) {
	part, err := Format(b, size, start, blocksize, volumeLabel)
	if err != nil {
		return nil, fmt.Errorf("failed to format exFAT: %w", err)
	}

	rootDir := &Directory{
		part:         part,
		firstCluster: part.rootDirectoryCluster,
		isContiguous: false,
		isRoot:       true,
		dataLength:   uint64(part.bytesPerCluster),
	}

	return &FileSystem{
		part:    part,
		rootDir: rootDir,
	}, nil
}

// Read mounts an existing exFAT filesystem from the provided storage.
func Read(b backend.Storage, size, start, blocksize int64) (*FileSystem, error) {
	if blocksize <= 0 {
		blocksize = BytesPerSectorDefault
	}
	secBuf := make([]byte, blocksize)
	if _, err := b.ReadAt(secBuf, start); err != nil {
		return nil, fmt.Errorf("failed to read boot sector: %w", err)
	}

	pbs, err := ParseBootSector(secBuf)
	if err != nil {
		return nil, err
	}

	bytesPerSector := pbs.BytesPerSector()
	bytesPerCluster := pbs.BytesPerCluster()

	part := &Partition{
		backend:                b,
		startOffset:            start,
		totalSize:              size,
		bytesPerSector:         bytesPerSector,
		bytesPerSectorShift:    pbs.BytesPerSectorShift,
		sectorsPerCluster:      pbs.SectorsPerCluster(),
		sectorsPerClusterShift: pbs.SectorsPerClusterShift,
		bytesPerCluster:        bytesPerCluster,
		sectorMask:             bytesPerSector - 1,
		clusterMask:            bytesPerCluster - 1,
		fatStartSector:         uint64(pbs.FatOffset),
		fatLength:              pbs.FatLength,
		clusterHeapStartSector: uint64(pbs.ClusterHeapOffset),
		clusterCount:           pbs.ClusterCount,
		rootDirectoryCluster:   pbs.RootDirectoryCluster,
	}

	rootDir := &Directory{
		part:         part,
		firstCluster: part.rootDirectoryCluster,
		isContiguous: false,
		isRoot:       true,
		dataLength:   uint64(bytesPerCluster),
	}

	// Read root directory entries to find Label, Bitmap, and Up-case table
	rootData, err := rootDir.ReadData()
	if err != nil {
		return nil, fmt.Errorf("failed to read root directory: %w", err)
	}

	numEntries := len(rootData) / BytesPerDirEntry
	for i := 0; i < numEntries; i++ {
		entryBytes := rootData[i*BytesPerDirEntry : (i+1)*BytesPerDirEntry]
		entryType := entryBytes[0]
		if entryType == ExFatTypeEndDir {
			break
		}
		switch entryType {
		case ExFatTypeLabel:
			dl := ParseDirLabel(entryBytes)
			part.volumeLabel = UTF16ToString(dl.Unicode[:dl.LabelLength])
		case ExFatTypeBitmap:
			dbm := ParseDirBitmap(entryBytes)
			part.bitmapStartCluster = dbm.FirstCluster
			part.bitmapSize = dbm.Size
		case ExFatTypeUpcase:
			dup := ParseDirUpcase(entryBytes)
			part.upcaseStartCluster = dup.FirstCluster
			part.upcaseSize = dup.Size
			part.upcaseChecksum = dup.Checksum
		}
	}

	if part.bitmapStartCluster < 2 {
		return nil, fmt.Errorf("allocation bitmap not found in root directory")
	}

	return &FileSystem{
		part:    part,
		rootDir: rootDir,
	}, nil
}

// Type returns filesystem.TypeExFAT.
func (fsys *FileSystem) Type() filesystem.Type {
	return filesystem.TypeExFAT
}

// Label returns the volume label of the filesystem.
func (fsys *FileSystem) Label() string {
	fsys.part.mu.Lock()
	defer fsys.part.mu.Unlock()
	return fsys.part.volumeLabel
}

// SetLabel sets the volume label of the filesystem.
func (fsys *FileSystem) SetLabel(label string) error {
	fsys.part.mu.Lock()
	defer fsys.part.mu.Unlock()

	data, err := fsys.rootDir.ReadData()
	if err != nil {
		return err
	}

	uLabel := StringToUTF16(label)
	if len(uLabel) > 11 {
		uLabel = uLabel[:11]
	}

	numEntries := len(data) / BytesPerDirEntry
	labelFound := false
	for i := 0; i < numEntries; i++ {
		entryBytes := data[i*BytesPerDirEntry : (i+1)*BytesPerDirEntry]
		if entryBytes[0] == ExFatTypeLabel || entryBytes[0] == (ExFatTypeLabel&0x7F) {
			if len(label) == 0 {
				data[i*BytesPerDirEntry] = ExFatTypeLabel & 0x7F // delete
			} else {
				dl := &DirLabel{
					Type:        ExFatTypeLabel,
					LabelLength: uint8(len(uLabel)),
				}
				copy(dl.Unicode[:], uLabel)
				copy(data[i*BytesPerDirEntry:(i+1)*BytesPerDirEntry], dl.Serialize())
			}
			labelFound = true
			break
		}
	}

	if !labelFound && len(label) > 0 {
		dl := &DirLabel{
			Type:        ExFatTypeLabel,
			LabelLength: uint8(len(uLabel)),
		}
		copy(dl.Unicode[:], uLabel)
		// insert label entry at the beginning or find free slot
		freeSlot := -1
		for i := 0; i < numEntries; i++ {
			t := data[i*BytesPerDirEntry]
			if t == ExFatTypeEndDir || (t&ExFatTypeUsed) == 0 {
				freeSlot = i
				break
			}
		}
		if freeSlot >= 0 {
			copy(data[freeSlot*BytesPerDirEntry:(freeSlot+1)*BytesPerDirEntry], dl.Serialize())
		} else {
			data = append(data, dl.Serialize()...)
		}
	}

	fsys.part.volumeLabel = label
	return fsys.rootDir.WriteData(data)
}

// resolveDir traverses the path from root directory and returns the parent directory and target basename.
func (fsys *FileSystem) resolveDir(p string) (*Directory, string, error) {
	parts := SplitPath(p)
	if len(parts) == 0 {
		return fsys.rootDir, "", nil
	}

	curDir := fsys.rootDir
	for i := 0; i < len(parts)-1; i++ {
		es, err := curDir.FindEntry(parts[i])
		if err != nil {
			return nil, "", fmt.Errorf("directory %s not found: %w", parts[i], err)
		}
		if !es.IsDirectory() {
			return nil, "", fmt.Errorf("%s is not a directory", parts[i])
		}
		nextDir := &Directory{
			part:         fsys.part,
			firstCluster: es.StreamEntry.FirstCluster,
			isContiguous: (es.StreamEntry.Flags & ExFatFlagContiguous) != 0,
			isRoot:       false,
			dataLength:   es.StreamEntry.DataLength,
			parent:       curDir,
			parentEntry:  es,
		}
		curDir = nextDir
	}

	return curDir, parts[len(parts)-1], nil
}

// Open opens a file for reading, implementing fs.FS.
func (fsys *FileSystem) Open(pathname string) (fs.File, error) {
	return fsys.OpenFile(pathname, os.O_RDONLY)
}

// OpenFile opens a file with the specified flags.
func (fsys *FileSystem) OpenFile(pathname string, flag int) (filesystem.File, error) {
	fsys.part.mu.Lock()
	defer fsys.part.mu.Unlock()

	parentDir, basename, err := fsys.resolveDir(pathname)
	if err != nil {
		return nil, err
	}
	if basename == "" {
		// Root directory
		es := &EntrySet{
			FileEntry: &DirFile{
				Attributes: ExFatAttribDirectory,
			},
			StreamEntry: &DirStream{
				FirstCluster: fsys.part.rootDirectoryCluster,
				DataLength:   uint64(fsys.part.bytesPerCluster),
			},
			Name: "/",
		}
		return &File{
			part:        fsys.part,
			parent:      parentDir,
			entrySet:    es,
			isReadWrite: false,
		}, nil
	}

	es, err := parentDir.FindEntry(basename)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if flag&os.O_CREATE == 0 {
				return nil, fmt.Errorf("file %s does not exist", pathname)
			}
			// Create new file
			newEs, err := CreateEntrySet(basename, false, 0, true, 0)
			if err != nil {
				return nil, err
			}
			if err := parentDir.AddEntrySet(newEs); err != nil {
				return nil, err
			}
			es = newEs
		} else {
			return nil, err
		}
	}

	isReadWrite := (flag&os.O_RDWR != 0) || (flag&os.O_WRONLY != 0)
	isAppend := (flag & os.O_APPEND) != 0

	fl := &File{
		part:        fsys.part,
		parent:      parentDir,
		entrySet:    es,
		isReadWrite: isReadWrite,
		isAppend:    isAppend,
	}

	if (flag&os.O_TRUNC) != 0 && isReadWrite && es.FileSize() > 0 {
		if err := fl.Truncate(0); err != nil {
			return nil, err
		}
	}

	if isAppend {
		fl.offset = es.FileSize()
	}

	return fl, nil
}

// ReadDir lists directory contents, implementing fs.ReadDirFS.
func (fsys *FileSystem) ReadDir(pathname string) ([]fs.DirEntry, error) {
	fsys.part.mu.Lock()
	defer fsys.part.mu.Unlock()

	parentDir, basename, err := fsys.resolveDir(pathname)
	if err != nil {
		return nil, err
	}

	targetDir := parentDir
	if basename != "" {
		es, err := parentDir.FindEntry(basename)
		if err != nil {
			return nil, err
		}
		if !es.IsDirectory() {
			return nil, fmt.Errorf("%s is not a directory", pathname)
		}
		targetDir = &Directory{
			part:         fsys.part,
			firstCluster: es.StreamEntry.FirstCluster,
			isContiguous: (es.StreamEntry.Flags & ExFatFlagContiguous) != 0,
			isRoot:       false,
			dataLength:   es.StreamEntry.DataLength,
			parent:       parentDir,
			parentEntry:  es,
		}
	}

	entries, err := targetDir.ReadEntries()
	if err != nil {
		return nil, err
	}

	var res []fs.DirEntry
	for _, e := range entries {
		var mode os.FileMode = 0o666
		if e.IsDirectory() {
			mode = os.ModeDir | 0o777
		}
		fi := &FileInfo{
			name:    e.Name,
			size:    e.FileSize(),
			mode:    mode,
			modTime: e.ModTime(),
			isDir:   e.IsDirectory(),
		}
		res = append(res, fi)
	}
	return res, nil
}

// ReadFile reads the full contents of a file, implementing fs.ReadFileFS.
func (fsys *FileSystem) ReadFile(pathname string) ([]byte, error) {
	f, err := fsys.Open(pathname)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Stat returns metadata for a file or directory, implementing fs.StatFS.
func (fsys *FileSystem) Stat(pathname string) (fs.FileInfo, error) {
	fsys.part.mu.Lock()
	defer fsys.part.mu.Unlock()

	parentDir, basename, err := fsys.resolveDir(pathname)
	if err != nil {
		return nil, err
	}
	if basename == "" {
		return &FileInfo{
			name:    "/",
			size:    int64(fsys.part.bytesPerCluster),
			mode:    os.ModeDir | 0o777,
			modTime: time.Now().UTC(),
			isDir:   true,
		}, nil
	}

	es, err := parentDir.FindEntry(basename)
	if err != nil {
		return nil, err
	}

	var mode os.FileMode = 0o666
	if es.IsDirectory() {
		mode = os.ModeDir | 0o777
	}
	return &FileInfo{
		name:    es.Name,
		size:    es.FileSize(),
		mode:    mode,
		modTime: es.ModTime(),
		isDir:   es.IsDirectory(),
	}, nil
}

// Mkdir creates a new subdirectory.
func (fsys *FileSystem) Mkdir(pathname string) error {
	fsys.part.mu.Lock()
	defer fsys.part.mu.Unlock()

	parentDir, basename, err := fsys.resolveDir(pathname)
	if err != nil {
		return err
	}
	if basename == "" {
		return fmt.Errorf("cannot recreate root directory")
	}

	if _, err := parentDir.FindEntry(basename); err == nil {
		return os.ErrExist
	}

	// Allocate 1 cluster for new directory
	newClus, err := fsys.part.BitmapFind(0, 1)
	if err != nil {
		return err
	}
	if newClus < 2 {
		return fmt.Errorf("disk full: cannot allocate cluster for directory")
	}
	if err := fsys.part.BitmapModify(newClus, 1, true); err != nil {
		return err
	}

	// Zero new cluster
	zeroData := make([]byte, fsys.part.bytesPerCluster)
	if err := fsys.part.WriteCluster(newClus, zeroData); err != nil {
		return err
	}

	es, err := CreateEntrySet(basename, true, newClus, true, uint64(fsys.part.bytesPerCluster))
	if err != nil {
		return err
	}

	return parentDir.AddEntrySet(es)
}

// Remove deletes a file or empty directory.
func (fsys *FileSystem) Remove(pathname string) error {
	fsys.part.mu.Lock()
	defer fsys.part.mu.Unlock()

	parentDir, basename, err := fsys.resolveDir(pathname)
	if err != nil {
		return err
	}
	if basename == "" {
		return fmt.Errorf("cannot remove root directory")
	}

	es, err := parentDir.FindEntry(basename)
	if err != nil {
		return err
	}

	if es.IsDirectory() {
		// Verify directory is empty
		subDir := &Directory{
			part:         fsys.part,
			firstCluster: es.StreamEntry.FirstCluster,
			isContiguous: (es.StreamEntry.Flags & ExFatFlagContiguous) != 0,
			isRoot:       false,
			dataLength:   es.StreamEntry.DataLength,
		}
		entries, err := subDir.ReadEntries()
		if err != nil {
			return err
		}
		if len(entries) > 0 {
			return fmt.Errorf("directory %s is not empty", pathname)
		}
	}

	// Free clusters
	if es.StreamEntry.FirstCluster >= 2 {
		if err := fsys.part.FreeChain(es.StreamEntry.FirstCluster); err != nil {
			return err
		}
	}

	return parentDir.RemoveEntrySet(es)
}

// Rename renames oldpath to newpath.
func (fsys *FileSystem) Rename(oldpath, newpath string) error {
	fsys.part.mu.Lock()
	defer fsys.part.mu.Unlock()

	oldParent, oldBase, err := fsys.resolveDir(oldpath)
	if err != nil {
		return err
	}
	oldEs, err := oldParent.FindEntry(oldBase)
	if err != nil {
		return err
	}

	newParent, newBase, err := fsys.resolveDir(newpath)
	if err != nil {
		return err
	}

	// Check if new path exists
	if existing, err := newParent.FindEntry(newBase); err == nil {
		if existing.IsDirectory() {
			return fmt.Errorf("destination %s already exists and is a directory", newpath)
		}
		// Overwrite existing file
		if err := fsys.part.FreeChain(existing.StreamEntry.FirstCluster); err != nil {
			return err
		}
		if err := newParent.RemoveEntrySet(existing); err != nil {
			return err
		}
	}

	// Remove from old directory
	if err := oldParent.RemoveEntrySet(oldEs); err != nil {
		return err
	}

	// Create new entry set with new name and existing cluster/size
	isDir := oldEs.IsDirectory()
	firstClus := oldEs.StreamEntry.FirstCluster
	isContiguous := (oldEs.StreamEntry.Flags & ExFatFlagContiguous) != 0
	size := oldEs.StreamEntry.DataLength

	newEs, err := CreateEntrySet(newBase, isDir, firstClus, isContiguous, size)
	if err != nil {
		return err
	}
	newEs.FileEntry.CreateDate = oldEs.FileEntry.CreateDate
	newEs.FileEntry.CreateTime = oldEs.FileEntry.CreateTime
	newEs.FileEntry.ModifyDate = oldEs.FileEntry.ModifyDate
	newEs.FileEntry.ModifyTime = oldEs.FileEntry.ModifyTime

	return newParent.AddEntrySet(newEs)
}

// Chtimes changes access and modification times.
func (fsys *FileSystem) Chtimes(pathname string, ctime, atime, mtime time.Time) error {
	fsys.part.mu.Lock()
	defer fsys.part.mu.Unlock()

	parentDir, basename, err := fsys.resolveDir(pathname)
	if err != nil {
		return err
	}
	if basename == "" {
		return nil
	}

	es, err := parentDir.FindEntry(basename)
	if err != nil {
		return err
	}

	if !ctime.IsZero() {
		cDate, cTime, cMs, cTz := TimeToExFat(ctime)
		es.FileEntry.CreateDate = cDate
		es.FileEntry.CreateTime = cTime
		es.FileEntry.CreateTimeMs = cMs
		es.FileEntry.CreateTimezone = cTz
	}
	if !atime.IsZero() {
		aDate, aTime, _, aTz := TimeToExFat(atime)
		es.FileEntry.AccessDate = aDate
		es.FileEntry.AccessTime = aTime
		es.FileEntry.AccessTimezone = aTz
	}
	if !mtime.IsZero() {
		mDate, mTime, mMs, mTz := TimeToExFat(mtime)
		es.FileEntry.ModifyDate = mDate
		es.FileEntry.ModifyTime = mTime
		es.FileEntry.ModifyTimeMs = mMs
		es.FileEntry.ModifyTimezone = mTz
	}

	return parentDir.UpdateEntrySet(es)
}

// Mknod, Link, Symlink, Chmod, Chown: unsupported in FAT/exFAT
func (fsys *FileSystem) Mknod(pathname string, mode uint32, dev int) error {
	return filesystem.ErrNotSupported
}

func (fsys *FileSystem) Link(oldpath, newpath string) error {
	return filesystem.ErrNotSupported
}

func (fsys *FileSystem) Symlink(oldpath, newpath string) error {
	return filesystem.ErrNotSupported
}

func (fsys *FileSystem) Chmod(name string, mode os.FileMode) error {
	return filesystem.ErrNotSupported
}

func (fsys *FileSystem) Chown(name string, uid, gid int) error {
	return filesystem.ErrNotSupported
}

// Close closes the filesystem.
func (fsys *FileSystem) Close() error {
	return nil
}
