package exfat

import (
	"encoding/binary"
	"fmt"
)

// Constants ported from SdFat / FsStructs.h
const (
	ExFatEOC uint32 = 0xFFFFFFFF

	ExFatTypeBitmap uint8 = 0x81
	ExFatTypeUpcase uint8 = 0x82
	ExFatTypeLabel  uint8 = 0x83

	ExFatTypeEndDir uint8 = 0x00
	ExFatTypeUsed   uint8 = 0x80
	ExFatTypeFile   uint8 = 0x85
	ExFatTypeStream uint8 = 0xC0
	ExFatTypeName   uint8 = 0xC1

	ExFatFlagAlways1    uint8 = 0x01
	ExFatFlagContiguous uint8 = 0x02

	ExFatMaxNameLength = 255

	ExFatAttribReadOnly  uint16 = 0x01
	ExFatAttribHidden    uint16 = 0x02
	ExFatAttribSystem    uint16 = 0x04
	ExFatAttribDirectory uint16 = 0x10
	ExFatAttribArchive   uint16 = 0x20

	BytesPerSectorDefault = 512
	BytesPerDirEntry      = 32

	BootSectorSignature uint16 = 0xAA55
)

// BootChecksum computes/updates the 32-bit boot checksum as defined in SdFat (exFatChecksum).
func BootChecksum(sum uint32, data byte) uint32 {
	return (sum << 31) + (sum >> 1) + uint32(data)
}

// BootSector represents the exFAT Main/Backup Boot Sector (ExFatBootSector / ExFatPbs_t in SdFat).
type BootSector struct {
	JmpInstruction         [3]byte
	OEMName                [8]byte
	PartitionOffset        uint64
	VolumeLength           uint64
	FatOffset              uint32
	FatLength              uint32
	ClusterHeapOffset      uint32
	ClusterCount           uint32
	RootDirectoryCluster   uint32
	VolumeSerialNumber     uint32
	FileSystemRevision     uint16
	VolumeFlags            uint16
	BytesPerSectorShift    uint8
	SectorsPerClusterShift uint8
	NumberOfFats           uint8
	DriveSelect            uint8
	PercentInUse           uint8
	Reserved               [7]byte
	BootCode               [390]byte
	Signature              uint16
}

func (bs *BootSector) BytesPerSector() uint32 {
	return 1 << bs.BytesPerSectorShift
}

func (bs *BootSector) SectorsPerCluster() uint32 {
	return 1 << bs.SectorsPerClusterShift
}

func (bs *BootSector) BytesPerCluster() uint32 {
	return 1 << (bs.BytesPerSectorShift + bs.SectorsPerClusterShift)
}

func ParseBootSector(b []byte) (*BootSector, error) {
	if len(b) < BytesPerSectorDefault {
		return nil, fmt.Errorf("boot sector buffer too small: %d bytes", len(b))
	}
	if string(b[3:11]) != "EXFAT   " {
		return nil, fmt.Errorf("invalid exFAT OEM name: %q", string(b[3:11]))
	}
	sig := binary.LittleEndian.Uint16(b[510:512])
	if sig != BootSectorSignature {
		return nil, fmt.Errorf("invalid boot sector signature: 0x%04X", sig)
	}

	bs := &BootSector{}
	copy(bs.JmpInstruction[:], b[0:3])
	copy(bs.OEMName[:], b[3:11])
	bs.PartitionOffset = binary.LittleEndian.Uint64(b[64:72])
	bs.VolumeLength = binary.LittleEndian.Uint64(b[72:80])
	bs.FatOffset = binary.LittleEndian.Uint32(b[80:84])
	bs.FatLength = binary.LittleEndian.Uint32(b[84:88])
	bs.ClusterHeapOffset = binary.LittleEndian.Uint32(b[88:92])
	bs.ClusterCount = binary.LittleEndian.Uint32(b[92:96])
	bs.RootDirectoryCluster = binary.LittleEndian.Uint32(b[96:100])
	bs.VolumeSerialNumber = binary.LittleEndian.Uint32(b[100:104])
	bs.FileSystemRevision = binary.LittleEndian.Uint16(b[104:106])
	bs.VolumeFlags = binary.LittleEndian.Uint16(b[106:108])
	bs.BytesPerSectorShift = b[108]
	bs.SectorsPerClusterShift = b[109]
	bs.NumberOfFats = b[110]
	bs.DriveSelect = b[111]
	bs.PercentInUse = b[112]
	copy(bs.Reserved[:], b[113:120])
	copy(bs.BootCode[:], b[120:510])
	bs.Signature = sig

	return bs, nil
}

func (bs *BootSector) Serialize(sectorSize int) []byte {
	buf := make([]byte, sectorSize)
	copy(buf[0:3], bs.JmpInstruction[:])
	copy(buf[3:11], bs.OEMName[:])
	// bytes 11..63 are zero (MustBeZero)
	binary.LittleEndian.PutUint64(buf[64:72], bs.PartitionOffset)
	binary.LittleEndian.PutUint64(buf[72:80], bs.VolumeLength)
	binary.LittleEndian.PutUint32(buf[80:84], bs.FatOffset)
	binary.LittleEndian.PutUint32(buf[84:88], bs.FatLength)
	binary.LittleEndian.PutUint32(buf[88:92], bs.ClusterHeapOffset)
	binary.LittleEndian.PutUint32(buf[92:96], bs.ClusterCount)
	binary.LittleEndian.PutUint32(buf[96:100], bs.RootDirectoryCluster)
	binary.LittleEndian.PutUint32(buf[100:104], bs.VolumeSerialNumber)
	binary.LittleEndian.PutUint16(buf[104:106], bs.FileSystemRevision)
	binary.LittleEndian.PutUint16(buf[106:108], bs.VolumeFlags)
	buf[108] = bs.BytesPerSectorShift
	buf[109] = bs.SectorsPerClusterShift
	buf[110] = bs.NumberOfFats
	buf[111] = bs.DriveSelect
	buf[112] = bs.PercentInUse
	copy(buf[113:120], bs.Reserved[:])
	copy(buf[120:510], bs.BootCode[:])
	binary.LittleEndian.PutUint16(buf[510:512], bs.Signature)
	return buf
}

// DirBitmap represents an Allocation Bitmap directory entry (DirBitmap_t in SdFat).
type DirBitmap struct {
	Type         uint8
	Flags        uint8
	Reserved     [18]byte
	FirstCluster uint32
	Size         uint64
}

func ParseDirBitmap(b []byte) *DirBitmap {
	d := &DirBitmap{
		Type:         b[0],
		Flags:        b[1],
		FirstCluster: binary.LittleEndian.Uint32(b[20:24]),
		Size:         binary.LittleEndian.Uint64(b[24:32]),
	}
	copy(d.Reserved[:], b[2:20])
	return d
}

func (d *DirBitmap) Serialize() []byte {
	b := make([]byte, BytesPerDirEntry)
	b[0] = d.Type
	b[1] = d.Flags
	copy(b[2:20], d.Reserved[:])
	binary.LittleEndian.PutUint32(b[20:24], d.FirstCluster)
	binary.LittleEndian.PutUint64(b[24:32], d.Size)
	return b
}

// DirUpcase represents an Up-case Table directory entry (DirUpcase_t in SdFat).
type DirUpcase struct {
	Type         uint8
	Reserved1    [3]byte
	Checksum     uint32
	Reserved2    [12]byte
	FirstCluster uint32
	Size         uint64
}

func ParseDirUpcase(b []byte) *DirUpcase {
	d := &DirUpcase{
		Type:         b[0],
		Checksum:     binary.LittleEndian.Uint32(b[4:8]),
		FirstCluster: binary.LittleEndian.Uint32(b[20:24]),
		Size:         binary.LittleEndian.Uint64(b[24:32]),
	}
	copy(d.Reserved1[:], b[1:4])
	copy(d.Reserved2[:], b[8:20])
	return d
}

func (d *DirUpcase) Serialize() []byte {
	b := make([]byte, BytesPerDirEntry)
	b[0] = d.Type
	copy(b[1:4], d.Reserved1[:])
	binary.LittleEndian.PutUint32(b[4:8], d.Checksum)
	copy(b[8:20], d.Reserved2[:])
	binary.LittleEndian.PutUint32(b[20:24], d.FirstCluster)
	binary.LittleEndian.PutUint64(b[24:32], d.Size)
	return b
}

// DirLabel represents a Volume Label directory entry (DirLabel_t in SdFat).
type DirLabel struct {
	Type        uint8
	LabelLength uint8
	Unicode     [11]uint16
	Reserved    [8]byte
}

func ParseDirLabel(b []byte) *DirLabel {
	d := &DirLabel{
		Type:        b[0],
		LabelLength: b[1],
	}
	for i := 0; i < 11; i++ {
		d.Unicode[i] = binary.LittleEndian.Uint16(b[2+i*2 : 4+i*2])
	}
	copy(d.Reserved[:], b[24:32])
	return d
}

func (d *DirLabel) Serialize() []byte {
	b := make([]byte, BytesPerDirEntry)
	b[0] = d.Type
	b[1] = d.LabelLength
	for i := 0; i < 11; i++ {
		binary.LittleEndian.PutUint16(b[2+i*2:4+i*2], d.Unicode[i])
	}
	copy(b[24:32], d.Reserved[:])
	return b
}

// DirFile represents a File directory entry (DirFile_t in SdFat).
type DirFile struct {
	Type           uint8
	SetCount       uint8
	SetChecksum    uint16
	Attributes     uint16
	Reserved1      uint16
	CreateTime     uint16
	CreateDate     uint16
	ModifyTime     uint16
	ModifyDate     uint16
	AccessTime     uint16
	AccessDate     uint16
	CreateTimeMs   uint8
	ModifyTimeMs   uint8
	CreateTimezone uint8
	ModifyTimezone uint8
	AccessTimezone uint8
	Reserved2      [7]byte
}

func ParseDirFile(b []byte) *DirFile {
	d := &DirFile{
		Type:           b[0],
		SetCount:       b[1],
		SetChecksum:    binary.LittleEndian.Uint16(b[2:4]),
		Attributes:     binary.LittleEndian.Uint16(b[4:6]),
		Reserved1:      binary.LittleEndian.Uint16(b[6:8]),
		CreateTime:     binary.LittleEndian.Uint16(b[8:10]),
		CreateDate:     binary.LittleEndian.Uint16(b[10:12]),
		ModifyTime:     binary.LittleEndian.Uint16(b[12:14]),
		ModifyDate:     binary.LittleEndian.Uint16(b[14:16]),
		AccessTime:     binary.LittleEndian.Uint16(b[16:18]),
		AccessDate:     binary.LittleEndian.Uint16(b[18:20]),
		CreateTimeMs:   b[20],
		ModifyTimeMs:   b[21],
		CreateTimezone: b[22],
		ModifyTimezone: b[23],
		AccessTimezone: b[24],
	}
	copy(d.Reserved2[:], b[25:32])
	return d
}

func (d *DirFile) Serialize() []byte {
	b := make([]byte, BytesPerDirEntry)
	b[0] = d.Type
	b[1] = d.SetCount
	binary.LittleEndian.PutUint16(b[2:4], d.SetChecksum)
	binary.LittleEndian.PutUint16(b[4:6], d.Attributes)
	binary.LittleEndian.PutUint16(b[6:8], d.Reserved1)
	binary.LittleEndian.PutUint16(b[8:10], d.CreateTime)
	binary.LittleEndian.PutUint16(b[10:12], d.CreateDate)
	binary.LittleEndian.PutUint16(b[12:14], d.ModifyTime)
	binary.LittleEndian.PutUint16(b[14:16], d.ModifyDate)
	binary.LittleEndian.PutUint16(b[16:18], d.AccessTime)
	binary.LittleEndian.PutUint16(b[18:20], d.AccessDate)
	b[20] = d.CreateTimeMs
	b[21] = d.ModifyTimeMs
	b[22] = d.CreateTimezone
	b[23] = d.ModifyTimezone
	b[24] = d.AccessTimezone
	copy(b[25:32], d.Reserved2[:])
	return b
}

// DirStream represents a Stream Extension directory entry (DirStream_t in SdFat).
type DirStream struct {
	Type         uint8
	Flags        uint8
	Reserved1    uint8
	NameLength   uint8
	NameHash     uint16
	Reserved2    uint16
	ValidLength  uint64
	Reserved3    uint32
	FirstCluster uint32
	DataLength   uint64
}

func ParseDirStream(b []byte) *DirStream {
	return &DirStream{
		Type:         b[0],
		Flags:        b[1],
		Reserved1:    b[2],
		NameLength:   b[3],
		NameHash:     binary.LittleEndian.Uint16(b[4:6]),
		Reserved2:    binary.LittleEndian.Uint16(b[6:8]),
		ValidLength:  binary.LittleEndian.Uint64(b[8:16]),
		Reserved3:    binary.LittleEndian.Uint32(b[16:20]),
		FirstCluster: binary.LittleEndian.Uint32(b[20:24]),
		DataLength:   binary.LittleEndian.Uint64(b[24:32]),
	}
}

func (d *DirStream) Serialize() []byte {
	b := make([]byte, BytesPerDirEntry)
	b[0] = d.Type
	b[1] = d.Flags
	b[2] = d.Reserved1
	b[3] = d.NameLength
	binary.LittleEndian.PutUint16(b[4:6], d.NameHash)
	binary.LittleEndian.PutUint16(b[6:8], d.Reserved2)
	binary.LittleEndian.PutUint64(b[8:16], d.ValidLength)
	binary.LittleEndian.PutUint32(b[16:20], d.Reserved3)
	binary.LittleEndian.PutUint32(b[20:24], d.FirstCluster)
	binary.LittleEndian.PutUint64(b[24:32], d.DataLength)
	return b
}

// DirName represents a File Name directory entry (DirName_t in SdFat).
type DirName struct {
	Type       uint8
	MustBeZero uint8
	Unicode    [15]uint16
}

func ParseDirName(b []byte) *DirName {
	d := &DirName{
		Type:       b[0],
		MustBeZero: b[1],
	}
	for i := 0; i < 15; i++ {
		d.Unicode[i] = binary.LittleEndian.Uint16(b[2+i*2 : 4+i*2])
	}
	return d
}

func (d *DirName) Serialize() []byte {
	b := make([]byte, BytesPerDirEntry)
	b[0] = d.Type
	b[1] = d.MustBeZero
	for i := 0; i < 15; i++ {
		binary.LittleEndian.PutUint16(b[2+i*2:4+i*2], d.Unicode[i])
	}
	return b
}
