package exfat

import (
	"fmt"
	"os"
	"time"

	"github.com/diskfs/go-diskfs/util/timestamp"
)

// EntrySet represents a file or directory's grouped directory entries
// (DirFile + DirStream + DirName[]), ported from SdFat / ExFatFile.
type EntrySet struct {
	FileEntry   *DirFile
	StreamEntry *DirStream
	NameEntries []*DirName
	Name        string
	DirOffset   uint32 // Byte offset within directory data
	SetCount    uint8
}

func (es *EntrySet) IsDirectory() bool {
	return (es.FileEntry.Attributes & ExFatAttribDirectory) != 0
}

func (es *EntrySet) ModTime() time.Time {
	return ExFatToTime(es.FileEntry.ModifyDate, es.FileEntry.ModifyTime, es.FileEntry.ModifyTimeMs, es.FileEntry.ModifyTimezone)
}

func (es *EntrySet) CreateTime() time.Time {
	return ExFatToTime(es.FileEntry.CreateDate, es.FileEntry.CreateTime, es.FileEntry.CreateTimeMs, es.FileEntry.CreateTimezone)
}

func (es *EntrySet) AccessTime() time.Time {
	return ExFatToTime(es.FileEntry.AccessDate, es.FileEntry.AccessTime, 0, es.FileEntry.AccessTimezone)
}

func (es *EntrySet) FileSize() int64 {
	return int64(es.StreamEntry.DataLength)
}

// ComputeChecksum calculates the 16-bit rotated checksum of all entries in the set,
// matching SdFat / ExFatFileWrite.cpp: exFatDirChecksum.
func (es *EntrySet) ComputeChecksum() uint16 {
	var entries [][]byte
	entries = append(entries, es.FileEntry.Serialize())
	entries = append(entries, es.StreamEntry.Serialize())
	for _, ne := range es.NameEntries {
		entries = append(entries, ne.Serialize())
	}

	var checksum uint16
	for _, entry := range entries {
		skip := (entry[0] == ExFatTypeFile)
		for j := 0; j < 32; j++ {
			if skip && (j == 2 || j == 3) {
				continue
			}
			checksum = ((checksum << 15) | (checksum >> 1)) + uint16(entry[j])
		}
	}
	return checksum
}

// Time conversion helpers
func TimeToExFat(t time.Time) (datePart, timePart uint16, msPart, tzPart uint8) {
	utc := t.UTC()
	year := utc.Year()
	if year < 1980 {
		year = 1980
	} else if year > 2107 {
		year = 2107
	}
	month := int(utc.Month())
	day := utc.Day()
	hour := utc.Hour()
	min := utc.Minute()
	sec := utc.Second()

	datePart = uint16(((year - 1980) << 9) | (month << 5) | day)
	timePart = uint16((hour << 11) | (min << 5) | (sec / 2))
	msPart = uint8(((sec % 2) * 1000 + (utc.Nanosecond() / 1_000_000)) / 10)
	tzPart = 0x80 // UTC / valid offset 0
	return
}

func ExFatToTime(datePart, timePart uint16, msPart, tzPart uint8) time.Time {
	if datePart == 0 {
		return time.Unix(0, 0).UTC()
	}
	year := int((datePart>>9)&0x7F) + 1980
	month := time.Month((datePart >> 5) & 0x0F)
	day := int(datePart & 0x1F)
	if month < 1 || month > 12 {
		month = 1
	}
	if day < 1 || day > 31 {
		day = 1
	}

	hour := int((timePart >> 11) & 0x1F)
	min := int((timePart >> 5) & 0x3F)
	sec := int(timePart&0x1F) * 2
	nsec := 0
	if msPart > 0 {
		sec += int(msPart) / 100
		nsec = (int(msPart) % 100) * 10_000_000
	}

	return time.Date(year, month, day, hour, min, sec, nsec, time.UTC)
}

// Directory represents an opened exFAT directory.
type Directory struct {
	part         *Partition
	firstCluster uint32
	isContiguous bool
	isRoot       bool
	dataLength   uint64
	parent       *Directory
	parentEntry  *EntrySet
}

// ReadData reads the full byte stream of the directory across its cluster chain.
func (d *Directory) ReadData() ([]byte, error) {
	if d.firstCluster < 2 {
		return nil, nil
	}
	var data []byte
	cur := d.firstCluster
	for {
		chunk, err := d.part.ReadCluster(cur)
		if err != nil {
			return nil, err
		}
		data = append(data, chunk...)
		if d.isContiguous {
			numClusters := uint32((d.dataLength + uint64(d.part.bytesPerCluster) - 1) / uint64(d.part.bytesPerCluster))
			if uint32(len(data)/int(d.part.bytesPerCluster)) >= numClusters {
				break
			}
			cur++
		} else {
			next, err := d.part.FatGet(cur)
			if err != nil {
				return nil, err
			}
			if next == 0 {
				break
			}
			cur = next
		}
	}
	return data, nil
}

// WriteData writes the directory byte stream back to its cluster chain.
func (d *Directory) WriteData(data []byte) error {
	bytesPerCluster := int(d.part.bytesPerCluster)
	numClusters := (len(data) + bytesPerCluster - 1) / bytesPerCluster
	cur := d.firstCluster

	for i := 0; i < numClusters; i++ {
		chunk := make([]byte, bytesPerCluster)
		start := i * bytesPerCluster
		end := start + bytesPerCluster
		if end > len(data) {
			end = len(data)
		}
		copy(chunk, data[start:end])

		if err := d.part.WriteCluster(cur, chunk); err != nil {
			return err
		}

		if d.isContiguous {
			cur++
		} else {
			if i+1 < numClusters {
				next, err := d.part.FatGet(cur)
				if err != nil {
					return err
				}
				cur = next
			}
		}
	}
	return nil
}

// ReadEntries parses all entry sets from the directory.
func (d *Directory) ReadEntries() ([]*EntrySet, error) {
	data, err := d.ReadData()
	if err != nil {
		return nil, err
	}

	var entrySets []*EntrySet
	totalEntries := len(data) / BytesPerDirEntry

	for i := 0; i < totalEntries; i++ {
		entryBytes := data[i*BytesPerDirEntry : (i+1)*BytesPerDirEntry]
		entryType := entryBytes[0]

		if entryType == ExFatTypeEndDir {
			break
		}
		if (entryType & ExFatTypeUsed) == 0 {
			continue // Deleted or unused entry
		}

		if entryType == ExFatTypeFile {
			fileEntry := ParseDirFile(entryBytes)
			setCount := int(fileEntry.SetCount)
			if i+setCount >= totalEntries {
				break
			}

			// Read stream entry (must follow file entry)
			streamBytes := data[(i+1)*BytesPerDirEntry : (i+2)*BytesPerDirEntry]
			if streamBytes[0] != ExFatTypeStream {
				continue
			}
			streamEntry := ParseDirStream(streamBytes)

			// Read name entries
			var nameEntries []*DirName
			var nameUTF16 []uint16
			for j := 2; j <= setCount; j++ {
				nameBytes := data[(i+j)*BytesPerDirEntry : (i+j+1)*BytesPerDirEntry]
				if nameBytes[0] != ExFatTypeName {
					break
				}
				ne := ParseDirName(nameBytes)
				nameEntries = append(nameEntries, ne)
				for _, u := range ne.Unicode {
					if u == 0 {
						break
					}
					nameUTF16 = append(nameUTF16, u)
				}
			}

			if len(nameUTF16) > int(streamEntry.NameLength) {
				nameUTF16 = nameUTF16[:streamEntry.NameLength]
			}

			es := &EntrySet{
				FileEntry:   fileEntry,
				StreamEntry: streamEntry,
				NameEntries: nameEntries,
				Name:        UTF16ToString(nameUTF16),
				DirOffset:   uint32(i * BytesPerDirEntry),
				SetCount:    fileEntry.SetCount,
			}
			entrySets = append(entrySets, es)
			i += setCount
		}
	}

	return entrySets, nil
}

// FindEntry locates an entry by filename case-insensitively.
func (d *Directory) FindEntry(name string) (*EntrySet, error) {
	entries, err := d.ReadEntries()
	if err != nil {
		return nil, err
	}
	targetU := StringToUTF16(name)
	for _, e := range entries {
		if CmpName(StringToUTF16(e.Name), targetU) {
			return e, nil
		}
	}
	return nil, os.ErrNotExist
}

// AddEntrySet adds a new entry set to the directory, allocating clusters if needed.
func (d *Directory) AddEntrySet(es *EntrySet) error {
	data, err := d.ReadData()
	if err != nil {
		return err
	}

	neededEntries := 1 + 1 + len(es.NameEntries)
	totalEntries := len(data) / BytesPerDirEntry

	// Search for contiguous free/deleted slots
	foundSlot := -1
	consec := 0
	for i := 0; i < totalEntries; i++ {
		t := data[i*BytesPerDirEntry]
		if t == ExFatTypeEndDir || (t&ExFatTypeUsed) == 0 {
			if consec == 0 {
				foundSlot = i
			}
			consec++
			if consec == neededEntries {
				break
			}
		} else {
			consec = 0
			foundSlot = -1
		}
	}

	if consec < neededEntries {
		// Append to the end
		foundSlot = totalEntries
		additionalBytes := (neededEntries - consec) * BytesPerDirEntry
		// Check if we need more clusters for the directory
		newTotalBytes := len(data) + additionalBytes
		bytesPerCluster := int(d.part.bytesPerCluster)
		if newTotalBytes > len(data) && (newTotalBytes > ((len(data)+bytesPerCluster-1)/bytesPerCluster)*bytesPerCluster || len(data) == 0) {
			// Allocate new cluster for directory
			newClus, err := d.part.BitmapFind(0, 1)
			if err != nil {
				return err
			}
			if newClus < 2 {
				return fmt.Errorf("disk full: cannot extend directory")
			}
			if err := d.part.BitmapModify(newClus, 1, true); err != nil {
				return err
			}
			// Zero new cluster
			zeroClus := make([]byte, bytesPerCluster)
			if err := d.part.WriteCluster(newClus, zeroClus); err != nil {
				return err
			}

			if d.firstCluster < 2 {
				d.firstCluster = newClus
				d.isContiguous = true
			} else {
				// Link in FAT
				lastCluster := d.firstCluster
				for {
					next, err := d.part.FatGet(lastCluster)
					if err != nil {
						return err
					}
					if next == 0 {
						break
					}
					lastCluster = next
				}
				if err := d.part.FatPut(lastCluster, newClus); err != nil {
					return err
				}
				if err := d.part.FatPut(newClus, ExFatEOC); err != nil {
					return err
				}
				d.isContiguous = false
			}
			data = append(data, zeroClus...)
		} else {
			padding := make([]byte, additionalBytes)
			data = append(data, padding...)
		}
	}

	es.DirOffset = uint32(foundSlot * BytesPerDirEntry)
	es.FileEntry.SetCount = uint8(1 + len(es.NameEntries))
	es.FileEntry.SetChecksum = es.ComputeChecksum()

	// Write entries into data buffer
	copy(data[es.DirOffset:es.DirOffset+BytesPerDirEntry], es.FileEntry.Serialize())
	copy(data[es.DirOffset+BytesPerDirEntry:es.DirOffset+2*BytesPerDirEntry], es.StreamEntry.Serialize())
	for i, ne := range es.NameEntries {
		off := es.DirOffset + uint32((2+i)*BytesPerDirEntry)
		copy(data[off:off+BytesPerDirEntry], ne.Serialize())
	}

	d.dataLength = uint64(len(data))
	return d.WriteData(data)
}

// UpdateEntrySet updates the directory entries of an existing entry set.
func (d *Directory) UpdateEntrySet(es *EntrySet) error {
	data, err := d.ReadData()
	if err != nil {
		return err
	}
	es.FileEntry.SetChecksum = es.ComputeChecksum()

	copy(data[es.DirOffset:es.DirOffset+BytesPerDirEntry], es.FileEntry.Serialize())
	copy(data[es.DirOffset+BytesPerDirEntry:es.DirOffset+2*BytesPerDirEntry], es.StreamEntry.Serialize())
	for i, ne := range es.NameEntries {
		off := es.DirOffset + uint32((2+i)*BytesPerDirEntry)
		copy(data[off:off+BytesPerDirEntry], ne.Serialize())
	}

	return d.WriteData(data)
}

// RemoveEntrySet marks the directory entries in an entry set as deleted.
func (d *Directory) RemoveEntrySet(es *EntrySet) error {
	data, err := d.ReadData()
	if err != nil {
		return err
	}

	// In exFAT, mark entries as deleted by clearing the MSB (bit 7) of entry type
	data[es.DirOffset] &= 0x7F
	data[es.DirOffset+BytesPerDirEntry] &= 0x7F
	for i := range es.NameEntries {
		off := es.DirOffset + uint32((2+i)*BytesPerDirEntry)
		data[off] &= 0x7F
	}

	return d.WriteData(data)
}

// CreateEntrySet builds an EntrySet for a new file or directory.
func CreateEntrySet(name string, isDir bool, firstCluster uint32, contiguous bool, size uint64) (*EntrySet, error) {
	if err := ValidateFilename(name); err != nil {
		return nil, err
	}
	uName := StringToUTF16(name)
	nameLen := len(uName)
	nameHash, err := HashName(uName)
	if err != nil {
		return nil, err
	}

	// 15 characters per DirName entry
	numNameEntries := (nameLen + 14) / 15
	nameEntries := make([]*DirName, numNameEntries)
	for i := 0; i < numNameEntries; i++ {
		ne := &DirName{
			Type: ExFatTypeName,
		}
		start := i * 15
		end := start + 15
		if end > nameLen {
			end = nameLen
		}
		copy(ne.Unicode[:], uName[start:end])
		nameEntries[i] = ne
	}

	now := timestamp.GetTime()
	cDate, cTime, cMs, cTz := TimeToExFat(now)
	mDate, mTime, mMs, mTz := TimeToExFat(now)
	aDate, aTime, _, aTz := TimeToExFat(now)

	var attribs uint16
	if isDir {
		attribs = ExFatAttribDirectory
	} else {
		attribs = ExFatAttribArchive
	}

	fileEntry := &DirFile{
		Type:           ExFatTypeFile,
		SetCount:       uint8(1 + numNameEntries),
		Attributes:     attribs,
		CreateDate:     cDate,
		CreateTime:     cTime,
		CreateTimeMs:   cMs,
		CreateTimezone: cTz,
		ModifyDate:     mDate,
		ModifyTime:     mTime,
		ModifyTimeMs:   mMs,
		ModifyTimezone: mTz,
		AccessDate:     aDate,
		AccessTime:     aTime,
		AccessTimezone: aTz,
	}

	flags := ExFatFlagAlways1
	if contiguous {
		flags |= ExFatFlagContiguous
	}

	streamEntry := &DirStream{
		Type:         ExFatTypeStream,
		Flags:        flags,
		NameLength:   uint8(nameLen),
		NameHash:     nameHash,
		ValidLength:  size,
		FirstCluster: firstCluster,
		DataLength:   size,
	}

	es := &EntrySet{
		FileEntry:   fileEntry,
		StreamEntry: streamEntry,
		NameEntries: nameEntries,
		Name:        name,
		SetCount:    uint8(1 + numNameEntries),
	}

	es.FileEntry.SetChecksum = es.ComputeChecksum()
	return es, nil
}
