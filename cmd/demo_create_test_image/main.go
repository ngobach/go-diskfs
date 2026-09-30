package main

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"

	diskfs "github.com/diskfs/go-diskfs"
	diskpkg "github.com/diskfs/go-diskfs/disk"
	"github.com/diskfs/go-diskfs/filesystem"
	"github.com/diskfs/go-diskfs/partition/gpt"
)

func main() {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("failed to get user home dir: %v", err)
	}

	diskPath := filepath.Join(homeDir, "Desktop", "exfat_disk.img")
	_ = os.Remove(diskPath) // Remove if previously exists

	fmt.Printf("Creating 1GB raw disk image at %s...\n", diskPath)

	diskSize := int64(1024 * 1024 * 1024) // 1 GiB
	sectorSize := diskfs.SectorSize512

	disk, err := diskfs.Create(diskPath, diskSize, sectorSize)
	if err != nil {
		log.Fatalf("failed to create disk image: %v", err)
	}

	// 1GB at 512 bytes/sector is 2,097,152 sectors.
	// In GPT:
	// Start at sector 2048 (1 MiB alignment).
	// Backup GPT table + header occupies the last 33 sectors (LBA 2,097,119 to 2,097,151).
	// So last usable sector is 2,097,152 - 34 = 2,097,118.
	totalSectors := uint64(diskSize / 512)
	partitionStart := uint64(2048)
	partitionEnd := totalSectors - 34

	table := &gpt.Table{
		LogicalSectorSize:  512,
		PhysicalSectorSize: 512,
		ProtectiveMBR:      true,
		Partitions: []*gpt.Partition{
			{
				Index: 1,
				Start: partitionStart,
				End:   partitionEnd,
				Type:  gpt.MicrosoftBasicData,
				Name:  "EXFAT_1GB",
			},
		},
	}

	fmt.Println("Applying GPT partition table...")
	if err := disk.Partition(table); err != nil {
		log.Fatalf("failed to write GPT partition table: %v", err)
	}

	fmt.Println("Formatting Partition 1 as exFAT...")
	spec := diskpkg.FilesystemSpec{
		Partition:   1,
		FSType:      filesystem.TypeExFAT,
		VolumeLabel: "EXFAT_1GB",
	}

	fsys, err := disk.CreateFilesystem(spec)
	if err != nil {
		log.Fatalf("failed to format exFAT partition: %v", err)
	}

	fmt.Println("Populating dummy files and directories...")

	// 1. Root README.txt
	writeFile(fsys, "/README.txt", []byte(
		"Welcome to your 1GB exFAT disk image!\n\n"+
			"This disk was created by go-diskfs with an exFAT driver\n"+
			"ported from Bill Greiman's SdFat library (pure Go).\n\n"+
			"Disk Layout:\n"+
			"- Partition Table: GPT (GUID Partition Table)\n"+
			"- Partition Type: Microsoft Basic Data (EBD0A0A2-B9E5-4433-87C0-68B6B72699C7)\n"+
			"- Filesystem: exFAT\n"+
			"- Volume Label: EXFAT_1GB\n",
	))

	// 2. Root hello.txt
	writeFile(fsys, "/hello.txt", []byte("Hello from exFAT on macOS Desktop!\n"))

	// 3. /documents directory with files
	if err := fsys.Mkdir("/documents"); err != nil {
		log.Fatalf("mkdir /documents failed: %v", err)
	}

	writeFile(fsys, "/documents/notes.txt", []byte(
		"Notes:\n"+
			"1. exFAT supports 64-bit file sizes (> 4GB).\n"+
			"2. exFAT supports UTF-16 filenames up to 255 characters.\n"+
			"3. exFAT uses cluster allocation bitmaps for high performance.\n",
	))

	writeFile(fsys, "/documents/sample.json", []byte(
		"{\n"+
			"  \"project\": \"go-diskfs\",\n"+
			"  \"filesystem\": \"exFAT\",\n"+
			"  \"status\": \"working\",\n"+
			"  \"size_bytes\": 1073741824\n"+
			"}\n",
	))

	// 4. /photos directory
	if err := fsys.Mkdir("/photos"); err != nil {
		log.Fatalf("mkdir /photos failed: %v", err)
	}

	writeFile(fsys, "/photos/image_list.txt", []byte(
		"Photo collection list:\n"+
			"- photo1_beach.jpg\n"+
			"- photo2_mountains.jpg\n"+
			"- photo3_city_sunset.jpg\n",
	))

	// 5. Multi-cluster 64 KiB data file
	largeData := make([]byte, 64*1024)
	for i := range largeData {
		largeData[i] = byte(i % 256)
	}
	writeFile(fsys, "/data_sample.bin", largeData)

	fmt.Println("Closing disk image...")
	if err := disk.Backend.Close(); err != nil {
		log.Printf("warning on close: %v", err)
	}

	// Verify disk can be reopened and read back
	fmt.Println("Verifying disk image can be reopened and read back...")
	d2, err := diskfs.Open(diskPath)
	if err != nil {
		log.Fatalf("failed to reopen disk: %v", err)
	}
	defer d2.Backend.Close()

	fsys2, err := d2.GetFilesystem(1)
	if err != nil {
		log.Fatalf("failed to get filesystem from partition 1: %v", err)
	}

	readmeData, err := fsys2.ReadFile("/README.txt")
	if err != nil {
		log.Fatalf("failed to read /README.txt: %v", err)
	}
	fmt.Printf("Verification succeeded! Read %d bytes from /README.txt:\n%s\n", len(readmeData), string(bytes.Split(readmeData, []byte("\n"))[0]))

	fmt.Printf("Disk image successfully created at: %s\n", diskPath)
}

func writeFile(fsys filesystem.FileSystem, path string, data []byte) {
	f, err := fsys.OpenFile(path, os.O_CREATE|os.O_RDWR)
	if err != nil {
		log.Fatalf("failed to open file %s: %v", path, err)
	}
	defer f.Close()

	if _, err := f.Write(data); err != nil {
		log.Fatalf("failed to write file %s: %v", path, err)
	}
}
