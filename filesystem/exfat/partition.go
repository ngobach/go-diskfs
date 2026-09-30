package exfat

import (
	"encoding/binary"
	"fmt"
	"sync"

	"github.com/diskfs/go-diskfs/backend"
)

// Partition manages the exFAT on-disk partition structures, allocation bitmap,
// and FAT table, ported from SdFat / src/ExFatLib/ExFatPartition.cpp.
type Partition struct {
	mu                      sync.Mutex
	backend                 backend.Storage
	startOffset             int64
	totalSize               int64
	bytesPerSector          uint32
	bytesPerSectorShift     uint8
	sectorsPerCluster       uint32
	sectorsPerClusterShift  uint8
	bytesPerCluster         uint32
	sectorMask              uint32
	clusterMask             uint32
	fatStartSector          uint64
	fatLength               uint32
	clusterHeapStartSector  uint64
	clusterCount            uint32
	rootDirectoryCluster    uint32
	bitmapStartCluster      uint32
	bitmapSize              uint64
	bitmapSearchStart       uint32 // m_bitmapStart in SdFat
	upcaseStartCluster      uint32
	upcaseSize              uint64
	upcaseChecksum          uint32
	volumeLabel             string
}

func (p *Partition) ClusterStartSector(cluster uint32) uint64 {
	return p.clusterHeapStartSector + uint64(cluster-2)*uint64(p.sectorsPerCluster)
}

func (p *Partition) ClusterByteOffset(cluster uint32) int64 {
	return p.startOffset + int64(p.ClusterStartSector(cluster))*int64(p.bytesPerSector)
}

func (p *Partition) ReadSector(sector uint64) ([]byte, error) {
	buf := make([]byte, p.bytesPerSector)
	offset := p.startOffset + int64(sector)*int64(p.bytesPerSector)
	n, err := p.backend.ReadAt(buf, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to read sector %d: %w", sector, err)
	}
	if uint32(n) < p.bytesPerSector {
		return nil, fmt.Errorf("partial read of sector %d: got %d bytes", sector, n)
	}
	return buf, nil
}

func (p *Partition) writeAt(b []byte, offset int64) (int, error) {
	w, err := p.backend.Writable()
	if err != nil {
		return 0, err
	}
	return w.WriteAt(b, offset)
}

func (p *Partition) WriteSector(sector uint64, b []byte) error {
	offset := p.startOffset + int64(sector)*int64(p.bytesPerSector)
	n, err := p.writeAt(b, offset)
	if err != nil {
		return fmt.Errorf("failed to write sector %d: %w", sector, err)
	}
	if uint32(n) < p.bytesPerSector {
		return fmt.Errorf("partial write of sector %d: wrote %d bytes", sector, n)
	}
	return nil
}

func (p *Partition) ReadCluster(cluster uint32) ([]byte, error) {
	buf := make([]byte, p.bytesPerCluster)
	offset := p.ClusterByteOffset(cluster)
	n, err := p.backend.ReadAt(buf, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to read cluster %d: %w", cluster, err)
	}
	if uint32(n) < p.bytesPerCluster {
		return nil, fmt.Errorf("partial read of cluster %d: got %d bytes", cluster, n)
	}
	return buf, nil
}

func (p *Partition) WriteCluster(cluster uint32, b []byte) error {
	offset := p.ClusterByteOffset(cluster)
	n, err := p.writeAt(b, offset)
	if err != nil {
		return fmt.Errorf("failed to write cluster %d: %w", cluster, err)
	}
	if uint32(n) < p.bytesPerCluster {
		return fmt.Errorf("partial write of cluster %d: wrote %d bytes", cluster, n)
	}
	return nil
}

// fatGet retrieves the next cluster in a chain, matching SdFat / ExFatPartition::fatGet.
// Returns (0, nil) if cluster is EXFAT_EOC, or (nextCluster, nil).
func (p *Partition) FatGet(cluster uint32) (uint32, error) {
	if cluster < 2 || cluster > (p.clusterCount+1) {
		return 0, fmt.Errorf("cluster number out of range: %d", cluster)
	}
	sector := p.fatStartSector + uint64(cluster>>(p.bytesPerSectorShift-2))
	secBuf, err := p.ReadSector(sector)
	if err != nil {
		return 0, err
	}
	offsetInSector := (cluster << 2) & p.sectorMask
	next := binary.LittleEndian.Uint32(secBuf[offsetInSector : offsetInSector+4])
	if next == ExFatEOC {
		return 0, nil
	}
	return next, nil
}

// fatPut updates a FAT entry, matching SdFat / ExFatPartition::fatPut.
func (p *Partition) FatPut(cluster, value uint32) error {
	if cluster < 2 || cluster > (p.clusterCount+1) {
		return fmt.Errorf("cluster number out of range: %d", cluster)
	}
	sector := p.fatStartSector + uint64(cluster>>(p.bytesPerSectorShift-2))
	secBuf, err := p.ReadSector(sector)
	if err != nil {
		return err
	}
	offsetInSector := (cluster << 2) & p.sectorMask
	binary.LittleEndian.PutUint32(secBuf[offsetInSector:offsetInSector+4], value)
	return p.WriteSector(sector, secBuf)
}

// bitmapFind finds count free consecutive clusters, matching SdFat / ExFatPartition::bitmapFind.
// Returns start cluster (>= 2), or 0 on error, or 1 if no space.
func (p *Partition) BitmapFind(cluster, count uint32) (uint32, error) {
	start := p.bitmapSearchStart
	if cluster != 0 {
		start = cluster - 2
	}
	if start >= p.clusterCount {
		start = 0
	}
	bgnAlloc := start
	endAlloc := start
	sectorSize := p.bytesPerSector

	startBitmapByteOffset := p.ClusterByteOffset(p.bitmapStartCluster)

	i := (start >> 3) & (sectorSize - 1)
	mask := byte(1 << (start & 7))

	for {
		sector := endAlloc >> (p.bytesPerSectorShift + 3)
		secBuf := make([]byte, sectorSize)
		secOffset := startBitmapByteOffset + int64(sector)*int64(sectorSize)
		if _, err := p.backend.ReadAt(secBuf, secOffset); err != nil {
			return 0, fmt.Errorf("failed reading bitmap: %w", err)
		}

		for ; i < sectorSize; i++ {
			for mask != 0 {
				endAlloc++
				if (mask & secBuf[i]) == 0 {
					if (endAlloc - bgnAlloc) == count {
						if cluster == 0 && count == 1 {
							p.bitmapSearchStart = bgnAlloc
						}
						return bgnAlloc + 2, nil
					}
				} else {
					bgnAlloc = endAlloc
				}
				if endAlloc == start {
					return 1, nil // No space
				}
				if endAlloc >= p.clusterCount {
					endAlloc = 0
					bgnAlloc = 0
					i = sectorSize
					break
				}
				mask <<= 1
			}
			mask = 1
		}
		i = 0
	}
}

// bitmapModify modifies the allocation bits for count clusters starting at cluster,
// matching SdFat / ExFatPartition::bitmapModify.
func (p *Partition) BitmapModify(cluster, count uint32, value bool) error {
	if cluster < 2 {
		return fmt.Errorf("invalid cluster to modify: %d", cluster)
	}
	start := cluster - 2
	if (start + count) > p.clusterCount {
		return fmt.Errorf("allocation beyond cluster count (%d + %d > %d)", start, count, p.clusterCount)
	}

	if value {
		if start <= p.bitmapSearchStart && p.bitmapSearchStart < (start+count) {
			if (start + count) < p.clusterCount {
				p.bitmapSearchStart = start + count
			} else {
				p.bitmapSearchStart = 0
			}
		}
	} else {
		if start < p.bitmapSearchStart {
			p.bitmapSearchStart = start
		}
	}

	startBitmapByteOffset := p.ClusterByteOffset(p.bitmapStartCluster)
	sectorSize := p.bytesPerSector
	mask := byte(1 << (start & 7))
	sector := start >> (p.bytesPerSectorShift + 3)
	i := (start >> 3) & p.sectorMask

	for {
		secBuf := make([]byte, sectorSize)
		secOffset := startBitmapByteOffset + int64(sector)*int64(sectorSize)
		if _, err := p.backend.ReadAt(secBuf, secOffset); err != nil {
			return fmt.Errorf("failed reading bitmap sector: %w", err)
		}

		for ; i < sectorSize; i++ {
			for mask != 0 {
				bitSet := (secBuf[i] & mask) != 0
				if value == bitSet {
					return fmt.Errorf("bitmap corruption: cluster %d expected bit state %v", start+2, !value)
				}
				secBuf[i] ^= mask
				count--
				if count == 0 {
					if _, err := p.writeAt(secBuf, secOffset); err != nil {
						return fmt.Errorf("failed writing bitmap sector: %w", err)
					}
					return nil
				}
				mask <<= 1
			}
			mask = 1
		}
		if _, err := p.writeAt(secBuf, secOffset); err != nil {
			return fmt.Errorf("failed writing bitmap sector: %w", err)
		}
		sector++
		i = 0
	}
}

// FreeChain frees the cluster chain and clears bitmap bits,
// matching SdFat / ExFatPartition::freeChain.
func (p *Partition) FreeChain(cluster uint32) error {
	if cluster < 2 {
		return nil
	}
	start := cluster
	for {
		next, err := p.FatGet(cluster)
		if err != nil {
			return err
		}
		if err := p.FatPut(cluster, 0); err != nil {
			return err
		}
		if next == 0 || (cluster+1) != next {
			if err := p.BitmapModify(start, cluster-start+1, false); err != nil {
				return err
			}
			start = next
		}
		if next == 0 {
			break
		}
		cluster = next
	}
	return nil
}

// FreeClusterCount calculates the number of free clusters,
// matching SdFat / ExFatPartition::freeClusterCount.
func (p *Partition) FreeClusterCount() (uint32, error) {
	startBitmapByteOffset := p.ClusterByteOffset(p.bitmapStartCluster)
	var usedCount uint32
	var nc uint32
	sectorSize := p.bytesPerSector
	sectorCount := (p.bitmapSize + uint64(sectorSize) - 1) / uint64(sectorSize)

	for sec := uint64(0); sec < sectorCount; sec++ {
		secBuf := make([]byte, sectorSize)
		secOffset := startBitmapByteOffset + int64(sec)*int64(sectorSize)
		if _, err := p.backend.ReadAt(secBuf, secOffset); err != nil {
			return 0, err
		}
		for _, b := range secBuf {
			if b == 0xFF {
				usedCount += 8
			} else if b != 0 {
				for mask := byte(1); mask != 0; mask <<= 1 {
					if (mask & b) != 0 {
						usedCount++
					}
				}
			}
			nc += 8
			if nc >= p.clusterCount {
				if usedCount > p.clusterCount {
					return 0, nil
				}
				return p.clusterCount - usedCount, nil
			}
		}
	}
	return p.clusterCount - usedCount, nil
}

// GetClusterChain returns all cluster indices in a file's chain.
func (p *Partition) GetClusterChain(firstCluster uint32, contiguous bool, count uint32) ([]uint32, error) {
	if firstCluster < 2 || count == 0 {
		return nil, nil
	}
	clusters := make([]uint32, 0, count)
	if contiguous {
		for i := uint32(0); i < count; i++ {
			clusters = append(clusters, firstCluster+i)
		}
		return clusters, nil
	}

	cur := firstCluster
	for {
		clusters = append(clusters, cur)
		next, err := p.FatGet(cur)
		if err != nil {
			return nil, err
		}
		if next == 0 {
			break
		}
		cur = next
	}
	return clusters, nil
}
