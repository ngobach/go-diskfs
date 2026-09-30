package exfat

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/bits"
	"time"

	"github.com/diskfs/go-diskfs/backend"
)

// Format formats a partition or disk image with exFAT,
// following the layout rules ported from SdFat / ExFatFormatter.cpp.
func Format(b backend.Storage, size, start, blocksize int64, volumeLabel string) (*Partition, error) {
	if blocksize <= 0 {
		blocksize = BytesPerSectorDefault
	}
	if blocksize != 512 && blocksize != 4096 {
		return nil, fmt.Errorf("unsupported sector size: %d (must be 512 or 4096)", blocksize)
	}

	bytesPerSector := uint32(blocksize)
	bytesPerSectorShift := uint8(bits.TrailingZeros32(bytesPerSector))

	sectorCount := uint64(size / int64(bytesPerSector))
	if sectorCount < 2048 {
		return nil, fmt.Errorf("device too small for exFAT: %d sectors (%d bytes)", sectorCount, size)
	}

	// Choose cluster size based on volume size
	var clusterBytes uint32
	switch {
	case size < 32*1024*1024:
		clusterBytes = 4 * 1024 // 4 KiB
	case size < 256*1024*1024:
		clusterBytes = 4 * 1024 // 4 KiB
	case size < 32*1024*1024*1024:
		clusterBytes = 32 * 1024 // 32 KiB
	default:
		clusterBytes = 128 * 1024 // 128 KiB
	}
	if clusterBytes < bytesPerSector {
		clusterBytes = bytesPerSector
	}

	sectorsPerCluster := clusterBytes / bytesPerSector
	sectorsPerClusterShift := uint8(bits.TrailingZeros32(sectorsPerCluster))

	// Main boot region is 12 sectors, backup is 12 sectors -> total 24 sectors minimum
	// We align FAT offset to sector 24, or 32 for clean boundary
	fatOffset := uint32(32)
	if bytesPerSector == 4096 {
		fatOffset = 24
	}

	// Estimate cluster count and fat length
	availableSectors := sectorCount - uint64(fatOffset)
	// Rough estimate: cluster count is approx availableSectors / sectorsPerCluster
	estClusterCount := uint32(availableSectors / uint64(sectorsPerCluster))
	fatLength := uint32((uint64(estClusterCount+2)*4 + uint64(bytesPerSector) - 1) / uint64(bytesPerSector))

	// Align cluster heap to cluster boundary or 32 sectors
	clusterHeapOffset := fatOffset + fatLength
	if rem := clusterHeapOffset % sectorsPerCluster; rem != 0 {
		clusterHeapOffset += (sectorsPerCluster - rem)
	}

	if uint64(clusterHeapOffset) >= sectorCount {
		return nil, fmt.Errorf("device too small for computed layout")
	}

	clusterCount := uint32((sectorCount - uint64(clusterHeapOffset)) / uint64(sectorsPerCluster))
	// Recompute exact fatLength
	fatLength = uint32((uint64(clusterCount+2)*4 + uint64(bytesPerSector) - 1) / uint64(bytesPerSector))

	// Volume length in sectors
	volumeLength := uint64(clusterHeapOffset) + uint64(clusterCount)*uint64(sectorsPerCluster)

	// Layout clusters in cluster heap:
	// Cluster 2: Allocation Bitmap
	bitmapCluster := uint32(2)
	bitmapSizeBytes := uint64((clusterCount + 7) / 8)
	bitmapClusterCount := uint32((bitmapSizeBytes + uint64(clusterBytes) - 1) / uint64(clusterBytes))

	// Upcase Table
	upcaseCluster := bitmapCluster + bitmapClusterCount
	upcaseData, upcaseCheck := GenerateUpcaseTable()
	upcaseSizeBytes := uint64(len(upcaseData))
	upcaseClusterCount := uint32((upcaseSizeBytes + uint64(clusterBytes) - 1) / uint64(clusterBytes))

	// Root directory
	rootCluster := upcaseCluster + upcaseClusterCount
	rootClusterCount := uint32(1)

	totalInitialClusters := bitmapClusterCount + upcaseClusterCount + rootClusterCount
	if totalInitialClusters > clusterCount {
		return nil, fmt.Errorf("filesystem too small to hold metadata clusters")
	}

	var volSerial uint32
	var serialBytes [4]byte
	if _, err := rand.Read(serialBytes[:]); err == nil {
		volSerial = binary.LittleEndian.Uint32(serialBytes[:])
	} else {
		volSerial = uint32(time.Now().Unix())
	}

	pbs := &BootSector{
		JmpInstruction:         [3]byte{0xEB, 0x76, 0x90},
		OEMName:                [8]byte{'E', 'X', 'F', 'A', 'T', ' ', ' ', ' '},
		PartitionOffset:        uint64(start / int64(bytesPerSector)),
		VolumeLength:           volumeLength,
		FatOffset:              fatOffset,
		FatLength:              fatLength,
		ClusterHeapOffset:      clusterHeapOffset,
		ClusterCount:           clusterCount,
		RootDirectoryCluster:   rootCluster,
		VolumeSerialNumber:     volSerial,
		FileSystemRevision:     0x0100,
		VolumeFlags:            0,
		BytesPerSectorShift:    bytesPerSectorShift,
		SectorsPerClusterShift: sectorsPerClusterShift,
		NumberOfFats:           1,
		DriveSelect:            0x80,
		PercentInUse:           uint8((uint64(totalInitialClusters) * 100) / uint64(clusterCount)),
		Signature:              BootSectorSignature,
	}

	// Build Boot Region sectors 0..11
	bootRegion := make([][]byte, 12)
	bootRegion[0] = pbs.Serialize(int(bytesPerSector))

	for i := 1; i <= 8; i++ {
		sec := make([]byte, bytesPerSector)
		binary.LittleEndian.PutUint16(sec[bytesPerSector-2:], BootSectorSignature)
		bootRegion[i] = sec
	}
	bootRegion[9] = make([]byte, bytesPerSector)  // OEM parameter sector
	bootRegion[10] = make([]byte, bytesPerSector) // Reserved sector

	// Compute boot checksum
	var chk uint32
	for secIdx := 0; secIdx <= 10; secIdx++ {
		for i, b := range bootRegion[secIdx] {
			if secIdx == 0 && (i == 106 || i == 107 || i == 112) {
				continue
			}
			chk = BootChecksum(chk, b)
		}
	}

	// Sector 11: Boot Checksum Sector
	chkSec := make([]byte, bytesPerSector)
	for i := 0; i < int(bytesPerSector); i += 4 {
		binary.LittleEndian.PutUint32(chkSec[i:i+4], chk)
	}
	bootRegion[11] = chkSec

	w, err := b.Writable()
	if err != nil {
		return nil, fmt.Errorf("storage not writable: %w", err)
	}

	// Write Main Boot Region (sectors 0..11)
	for i := 0; i < 12; i++ {
		offset := start + int64(i)*int64(bytesPerSector)
		if _, err := w.WriteAt(bootRegion[i], offset); err != nil {
			return nil, fmt.Errorf("failed to write main boot sector %d: %w", i, err)
		}
	}

	// Write Backup Boot Region (sectors 12..23)
	for i := 0; i < 12; i++ {
		offset := start + int64(12+i)*int64(bytesPerSector)
		if _, err := w.WriteAt(bootRegion[i], offset); err != nil {
			return nil, fmt.Errorf("failed to write backup boot sector %d: %w", i, err)
		}
	}

	part := &Partition{
		backend:                b,
		startOffset:            start,
		totalSize:              size,
		bytesPerSector:         bytesPerSector,
		bytesPerSectorShift:    bytesPerSectorShift,
		sectorsPerCluster:      sectorsPerCluster,
		sectorsPerClusterShift: sectorsPerClusterShift,
		bytesPerCluster:        clusterBytes,
		sectorMask:             bytesPerSector - 1,
		clusterMask:            clusterBytes - 1,
		fatStartSector:         uint64(fatOffset),
		fatLength:              fatLength,
		clusterHeapStartSector: uint64(clusterHeapOffset),
		clusterCount:           clusterCount,
		rootDirectoryCluster:   rootCluster,
		bitmapStartCluster:     bitmapCluster,
		bitmapSize:             bitmapSizeBytes,
		bitmapSearchStart:      totalInitialClusters,
		upcaseStartCluster:     upcaseCluster,
		upcaseSize:             upcaseSizeBytes,
		upcaseChecksum:         upcaseCheck,
		volumeLabel:            volumeLabel,
	}

	// Initialize FAT Table:
	// FAT[0] = 0xFFFFFFF8, FAT[1] = 0xFFFFFFFF
	// The metadata clusters can be contiguous so FAT entries for them can be set.
	fatSecBuf := make([]byte, bytesPerSector)
	binary.LittleEndian.PutUint32(fatSecBuf[0:4], 0xFFFFFFF8)
	binary.LittleEndian.PutUint32(fatSecBuf[4:8], 0xFFFFFFFF)
	if err := part.WriteSector(uint64(fatOffset), fatSecBuf); err != nil {
		return nil, fmt.Errorf("failed to write FAT sector 0: %w", err)
	}

	// Write Allocation Bitmap
	bitmapData := make([]byte, bitmapClusterCount*clusterBytes)
	for c := uint32(0); c < totalInitialClusters; c++ {
		byteIdx := c / 8
		bitIdx := c % 8
		bitmapData[byteIdx] |= (1 << bitIdx)
	}
	for i := uint32(0); i < bitmapClusterCount; i++ {
		chunk := bitmapData[i*clusterBytes : (i+1)*clusterBytes]
		if err := part.WriteCluster(bitmapCluster+i, chunk); err != nil {
			return nil, fmt.Errorf("failed to write bitmap cluster: %w", err)
		}
	}

	// Write Up-case Table
	upcaseFull := make([]byte, upcaseClusterCount*clusterBytes)
	copy(upcaseFull, upcaseData)
	for i := uint32(0); i < upcaseClusterCount; i++ {
		chunk := upcaseFull[i*clusterBytes : (i+1)*clusterBytes]
		if err := part.WriteCluster(upcaseCluster+i, chunk); err != nil {
			return nil, fmt.Errorf("failed to write upcase cluster: %w", err)
		}
	}

	// Write Root Directory Cluster
	rootDirData := make([]byte, clusterBytes)
	dirOffset := 0

	// 1. Volume label if present
	if volumeLabel != "" {
		uLabel := StringToUTF16(volumeLabel)
		if len(uLabel) > 11 {
			uLabel = uLabel[:11]
		}
		labelEntry := &DirLabel{
			Type:        ExFatTypeLabel,
			LabelLength: uint8(len(uLabel)),
		}
		copy(labelEntry.Unicode[:], uLabel)
		copy(rootDirData[dirOffset:dirOffset+BytesPerDirEntry], labelEntry.Serialize())
		dirOffset += BytesPerDirEntry
	}

	// 2. Allocation bitmap entry
	bmEntry := &DirBitmap{
		Type:         ExFatTypeBitmap,
		Flags:        0,
		FirstCluster: bitmapCluster,
		Size:         bitmapSizeBytes,
	}
	copy(rootDirData[dirOffset:dirOffset+BytesPerDirEntry], bmEntry.Serialize())
	dirOffset += BytesPerDirEntry

	// 3. Up-case table entry
	upEntry := &DirUpcase{
		Type:         ExFatTypeUpcase,
		Checksum:     upcaseCheck,
		FirstCluster: upcaseCluster,
		Size:         upcaseSizeBytes,
	}
	copy(rootDirData[dirOffset:dirOffset+BytesPerDirEntry], upEntry.Serialize())
	dirOffset += BytesPerDirEntry

	// Write root directory
	if err := part.WriteCluster(rootCluster, rootDirData); err != nil {
		return nil, fmt.Errorf("failed to write root directory cluster: %w", err)
	}

	return part, nil
}
