package exfat

import (
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/ngobach/go-diskfs/filesystem"
	"github.com/ngobach/go-diskfs/util/timestamp"
)

var _ filesystem.File = (*File)(nil)
var _ fs.File = (*File)(nil)

// DiskRange represents a contiguous area on disk occupied by a file.
type DiskRange struct {
	Offset uint64
	Length uint64
}

// File represents an open file on an exFAT filesystem,
// ported from SdFat / ExFatFile.cpp & ExFatFileWrite.cpp.
type File struct {
	part        *Partition
	parent      *Directory
	entrySet    *EntrySet
	offset      int64
	isReadWrite bool
	isAppend    bool
	closed      bool
}

func (fl *File) isContiguous() bool {
	return (fl.entrySet.StreamEntry.Flags & ExFatFlagContiguous) != 0
}

func (fl *File) firstCluster() uint32 {
	return fl.entrySet.StreamEntry.FirstCluster
}

func (fl *File) dataLength() uint64 {
	return fl.entrySet.StreamEntry.DataLength
}

// Stat returns file metadata.
func (fl *File) Stat() (fs.FileInfo, error) {
	if fl.closed {
		return nil, os.ErrClosed
	}
	var mode os.FileMode = 0o666
	if fl.entrySet.IsDirectory() {
		mode = os.ModeDir | 0o777
	}
	return &FileInfo{
		name:    fl.entrySet.Name,
		size:    fl.entrySet.FileSize(),
		mode:    mode,
		modTime: fl.entrySet.ModTime(),
		isDir:   fl.entrySet.IsDirectory(),
	}, nil
}

// Read reads up to len(p) bytes from the file.
func (fl *File) Read(p []byte) (int, error) {
	if fl.closed {
		return 0, os.ErrClosed
	}
	if len(p) == 0 {
		return 0, nil
	}
	fileSize := fl.entrySet.FileSize()
	if fl.offset >= fileSize {
		return 0, io.EOF
	}

	bytesPerCluster := int64(fl.part.bytesPerCluster)
	totalRead := 0

	for totalRead < len(p) && fl.offset < fileSize {
		clusterIndex := uint32(fl.offset / bytesPerCluster)
		clusterOffset := fl.offset % bytesPerCluster

		// Resolve actual cluster number
		var cluster uint32
		if fl.isContiguous() {
			cluster = fl.firstCluster() + clusterIndex
		} else {
			cur := fl.firstCluster()
			for i := uint32(0); i < clusterIndex; i++ {
				next, err := fl.part.FatGet(cur)
				if err != nil {
					return totalRead, err
				}
				if next == 0 {
					return totalRead, io.ErrUnexpectedEOF
				}
				cur = next
			}
			cluster = cur
		}

		clusterData, err := fl.part.ReadCluster(cluster)
		if err != nil {
			return totalRead, err
		}

		bytesAvailableInCluster := bytesPerCluster - clusterOffset
		bytesLeftInFile := fileSize - fl.offset
		toRead := int64(len(p) - totalRead)
		if toRead > bytesAvailableInCluster {
			toRead = bytesAvailableInCluster
		}
		if toRead > bytesLeftInFile {
			toRead = bytesLeftInFile
		}

		copy(p[totalRead:totalRead+int(toRead)], clusterData[clusterOffset:clusterOffset+toRead])
		totalRead += int(toRead)
		fl.offset += toRead
	}

	return totalRead, nil
}

// addCluster allocates and appends a single cluster to the file,
// ported from SdFat / ExFatFile::addCluster.
func (fl *File) addCluster() error {
	fl.part.mu.Lock()
	defer fl.part.mu.Unlock()

	lastCluster := fl.firstCluster()
	var curCluster uint32
	if lastCluster == 0 {
		curCluster = 0
	} else if fl.isContiguous() {
		clusterCount := uint32((fl.dataLength() + uint64(fl.part.bytesPerCluster) - 1) / uint64(fl.part.bytesPerCluster))
		curCluster = lastCluster + clusterCount - 1
	} else {
		cur := lastCluster
		for {
			next, err := fl.part.FatGet(cur)
			if err != nil {
				return err
			}
			if next == 0 {
				break
			}
			cur = next
		}
		curCluster = cur
	}

	searchStart := uint32(0)
	if curCluster != 0 {
		searchStart = curCluster + 1
	}
	find, err := fl.part.BitmapFind(searchStart, 1)
	if err != nil {
		return err
	}
	if find < 2 {
		return fmt.Errorf("disk full: cannot allocate cluster")
	}
	if err := fl.part.BitmapModify(find, 1, true); err != nil {
		return err
	}

	// Zero out newly allocated cluster
	zeroData := make([]byte, fl.part.bytesPerCluster)
	if err := fl.part.WriteCluster(find, zeroData); err != nil {
		return err
	}

	if curCluster == 0 {
		fl.entrySet.StreamEntry.FirstCluster = find
		fl.entrySet.StreamEntry.Flags |= ExFatFlagContiguous
		return nil
	}

	if fl.isContiguous() {
		if find == (curCluster + 1) {
			// Still contiguous!
			return nil
		}
		// No longer contiguous: build FAT chain for previously allocated clusters
		fl.entrySet.StreamEntry.Flags &= ^ExFatFlagContiguous
		clusterCount := uint32((fl.dataLength() + uint64(fl.part.bytesPerCluster) - 1) / uint64(fl.part.bytesPerCluster))
		for c := uint32(0); c < clusterCount-1; c++ {
			if err := fl.part.FatPut(lastCluster+c, lastCluster+c+1); err != nil {
				return err
			}
		}
		if err := fl.part.FatPut(curCluster, find); err != nil {
			return err
		}
		return fl.part.FatPut(find, ExFatEOC)
	}

	// Link previous cluster to new cluster in FAT
	if err := fl.part.FatPut(curCluster, find); err != nil {
		return err
	}
	return fl.part.FatPut(find, ExFatEOC)
}

// Write writes data to the file, extending clusters as needed.
func (fl *File) Write(p []byte) (int, error) {
	if fl.closed {
		return 0, os.ErrClosed
	}
	if !fl.isReadWrite {
		return 0, fmt.Errorf("file not opened for writing")
	}
	if len(p) == 0 {
		return 0, nil
	}

	if fl.isAppend {
		fl.offset = fl.entrySet.FileSize()
	}

	bytesPerCluster := int64(fl.part.bytesPerCluster)
	totalWritten := 0

	for totalWritten < len(p) {
		clusterIndex := uint32(fl.offset / bytesPerCluster)
		clusterOffset := fl.offset % bytesPerCluster

		// Check if we need to allocate more clusters
		currentCapacity := int64(0)
		if fl.firstCluster() != 0 {
			numClusters := (int64(fl.dataLength()) + bytesPerCluster - 1) / bytesPerCluster
			currentCapacity = numClusters * bytesPerCluster
		}

		for fl.offset >= currentCapacity || fl.firstCluster() == 0 {
			if err := fl.addCluster(); err != nil {
				return totalWritten, err
			}
			numClusters := (int64(fl.dataLength()) + bytesPerCluster - 1) / bytesPerCluster
			if fl.dataLength() == 0 {
				numClusters = 1
			} else {
				numClusters++
			}
			currentCapacity = numClusters * bytesPerCluster
		}

		// Resolve actual cluster
		var cluster uint32
		if fl.isContiguous() {
			cluster = fl.firstCluster() + clusterIndex
		} else {
			cur := fl.firstCluster()
			for i := uint32(0); i < clusterIndex; i++ {
				next, err := fl.part.FatGet(cur)
				if err != nil {
					return totalWritten, err
				}
				if next == 0 {
					return totalWritten, fmt.Errorf("unexpected end of cluster chain")
				}
				cur = next
			}
			cluster = cur
		}

		clusterData, err := fl.part.ReadCluster(cluster)
		if err != nil {
			return totalWritten, err
		}

		bytesAvailableInCluster := bytesPerCluster - clusterOffset
		toWrite := int64(len(p) - totalWritten)
		if toWrite > bytesAvailableInCluster {
			toWrite = bytesAvailableInCluster
		}

		copy(clusterData[clusterOffset:clusterOffset+toWrite], p[totalWritten:totalWritten+int(toWrite)])
		if err := fl.part.WriteCluster(cluster, clusterData); err != nil {
			return totalWritten, err
		}

		totalWritten += int(toWrite)
		fl.offset += toWrite

		if fl.offset > int64(fl.dataLength()) {
			fl.entrySet.StreamEntry.DataLength = uint64(fl.offset)
			fl.entrySet.StreamEntry.ValidLength = uint64(fl.offset)
		}
	}

	// Update modify timestamp
	now := timestamp.GetTime()
	mDate, mTime, mMs, mTz := TimeToExFat(now)
	fl.entrySet.FileEntry.ModifyDate = mDate
	fl.entrySet.FileEntry.ModifyTime = mTime
	fl.entrySet.FileEntry.ModifyTimeMs = mMs
	fl.entrySet.FileEntry.ModifyTimezone = mTz

	// Sync entry set to parent directory
	if err := fl.parent.UpdateEntrySet(fl.entrySet); err != nil {
		return totalWritten, err
	}

	return totalWritten, nil
}

// Seek sets the offset for the next Read or Write.
func (fl *File) Seek(offset int64, whence int) (int64, error) {
	if fl.closed {
		return 0, os.ErrClosed
	}
	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset = fl.offset + offset
	case io.SeekEnd:
		newOffset = fl.entrySet.FileSize() + offset
	default:
		return 0, fmt.Errorf("invalid whence: %d", whence)
	}

	if newOffset < 0 {
		return 0, fmt.Errorf("negative seek offset: %d", newOffset)
	}
	fl.offset = newOffset
	return fl.offset, nil
}

// Truncate truncates the file to the specified size.
func (fl *File) Truncate(size int64) error {
	if fl.closed {
		return os.ErrClosed
	}
	if !fl.isReadWrite {
		return fmt.Errorf("file not opened for writing")
	}
	if size < 0 {
		return fmt.Errorf("negative truncate size: %d", size)
	}

	bytesPerCluster := int64(fl.part.bytesPerCluster)
	newClusters := uint32((size + bytesPerCluster - 1) / bytesPerCluster)

	clusters, err := fl.GetClusterChain()
	if err != nil {
		return err
	}

	if uint32(len(clusters)) > newClusters {
		// Free trailing clusters
		trailing := clusters[newClusters:]
		for _, c := range trailing {
			if err := fl.part.FatPut(c, 0); err != nil {
				return err
			}
			if err := fl.part.BitmapModify(c, 1, false); err != nil {
				return err
			}
		}
		if newClusters > 0 {
			if !fl.isContiguous() {
				if err := fl.part.FatPut(clusters[newClusters-1], ExFatEOC); err != nil {
					return err
				}
			}
		} else {
			fl.entrySet.StreamEntry.FirstCluster = 0
		}
	}

	fl.entrySet.StreamEntry.DataLength = uint64(size)
	fl.entrySet.StreamEntry.ValidLength = uint64(size)
	if fl.offset > size {
		fl.offset = size
	}

	return fl.parent.UpdateEntrySet(fl.entrySet)
}

// Close closes the file handle.
func (fl *File) Close() error {
	if fl.closed {
		return nil
	}
	fl.closed = true
	return fl.parent.UpdateEntrySet(fl.entrySet)
}

// GetClusterChain returns the full cluster chain for the file.
func (fl *File) GetClusterChain() ([]uint32, error) {
	if fl.closed {
		return nil, os.ErrClosed
	}
	if fl.firstCluster() == 0 || fl.dataLength() == 0 {
		return []uint32{}, nil
	}
	count := uint32((fl.dataLength() + uint64(fl.part.bytesPerCluster) - 1) / uint64(fl.part.bytesPerCluster))
	return fl.part.GetClusterChain(fl.firstCluster(), fl.isContiguous(), count)
}

// GetDiskRanges returns the disk ranges occupied by the file.
func (fl *File) GetDiskRanges() ([]DiskRange, error) {
	clusters, err := fl.GetClusterChain()
	if err != nil {
		return nil, err
	}
	if len(clusters) == 0 {
		return []DiskRange{}, nil
	}

	bytesPerCluster := uint64(fl.part.bytesPerCluster)
	var ranges []DiskRange
	curRange := DiskRange{
		Offset: uint64(fl.part.ClusterByteOffset(clusters[0])),
		Length: bytesPerCluster,
	}

	for i := 1; i < len(clusters); i++ {
		if clusters[i] == clusters[i-1]+1 {
			curRange.Length += bytesPerCluster
		} else {
			ranges = append(ranges, curRange)
			curRange = DiskRange{
				Offset: uint64(fl.part.ClusterByteOffset(clusters[i])),
				Length: bytesPerCluster,
			}
		}
	}
	ranges = append(ranges, curRange)
	return ranges, nil
}
