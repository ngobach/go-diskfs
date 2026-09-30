package exfat_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	diskfs "github.com/ngobach/go-diskfs"
	"github.com/ngobach/go-diskfs/backend"
	"github.com/ngobach/go-diskfs/backend/file"
	"github.com/ngobach/go-diskfs/disk"
	"github.com/ngobach/go-diskfs/filesystem"
	"github.com/ngobach/go-diskfs/filesystem/exfat"
)

func createTestBackend(t *testing.T, size int64) backend.Storage {
	t.Helper()
	imgPath := filepath.Join(t.TempDir(), "exfat_test.img")
	bk, err := file.CreateFromPath(imgPath, size)
	if err != nil {
		t.Fatalf("failed to create test backend: %v", err)
	}
	return bk
}

func TestUpcase(t *testing.T) {
	table, chk := exfat.GenerateUpcaseTable()
	if len(table) == 0 {
		t.Fatal("empty upcase table generated")
	}
	if chk == 0 {
		t.Fatal("zero checksum for upcase table")
	}

	// Verify standard ASCII casing
	if exfat.StringToUTF16("abc")[0] == exfat.StringToUTF16("ABC")[0] {
		t.Fatal("unexpected UTF16 encoding")
	}
	if !exfat.CmpName(exfat.StringToUTF16("HelloWorld.txt"), exfat.StringToUTF16("helloworld.TXT")) {
		t.Fatal("case-insensitive comparison failed for ASCII")
	}
	if exfat.CmpName(exfat.StringToUTF16("HelloWorld.txt"), exfat.StringToUTF16("Different.txt")) {
		t.Fatal("comparison should fail for different names")
	}
}

func TestNameHash(t *testing.T) {
	h1, err := exfat.HashName(exfat.StringToUTF16("TEST.TXT"))
	if err != nil {
		t.Fatalf("unexpected error hashing name: %v", err)
	}
	h2, err := exfat.HashName(exfat.StringToUTF16("test.txt"))
	if err != nil {
		t.Fatalf("unexpected error hashing name: %v", err)
	}
	if h1 != h2 {
		t.Fatalf("expected hash for TEST.TXT (%d) and test.txt (%d) to match", h1, h2)
	}
}

func TestCreateAndMount(t *testing.T) {
	size := int64(20 * 1024 * 1024) // 20 MB
	bk := createTestBackend(t, size)

	label := "MYEXFAT"
	fsys, err := exfat.Create(bk, size, 0, 512, label)
	if err != nil {
		t.Fatalf("failed to create exFAT filesystem: %v", err)
	}

	if fsys.Type() != filesystem.TypeExFAT {
		t.Fatalf("expected TypeExFAT, got %v", fsys.Type())
	}

	if fsys.Label() != label {
		t.Fatalf("expected label %q, got %q", label, fsys.Label())
	}

	// Re-mount using Read
	mounted, err := exfat.Read(bk, size, 0, 512)
	if err != nil {
		t.Fatalf("failed to read created exFAT filesystem: %v", err)
	}

	if mounted.Type() != filesystem.TypeExFAT {
		t.Fatalf("expected mounted TypeExFAT, got %v", mounted.Type())
	}

	if mounted.Label() != label {
		t.Fatalf("expected mounted label %q, got %q", label, mounted.Label())
	}
}

func TestFileReadWrite(t *testing.T) {
	size := int64(20 * 1024 * 1024)
	bk := createTestBackend(t, size)

	fsys, err := exfat.Create(bk, size, 0, 512, "FILES")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Create and write a file
	filePath := "/hello.txt"
	f, err := fsys.OpenFile(filePath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("open file failed: %v", err)
	}

	content := []byte("Hello, exFAT world!")
	n, err := f.Write(content)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if n != len(content) {
		t.Fatalf("expected %d bytes written, got %d", len(content), n)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// Read back file
	readData, err := fsys.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read file failed: %v", err)
	}
	if !bytes.Equal(readData, content) {
		t.Fatalf("read mismatch: expected %q, got %q", string(content), string(readData))
	}

	// Stat file
	info, err := fsys.Stat(filePath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if info.Name() != "hello.txt" {
		t.Fatalf("expected name hello.txt, got %s", info.Name())
	}
	if info.Size() != int64(len(content)) {
		t.Fatalf("expected size %d, got %d", len(content), info.Size())
	}
	if info.IsDir() {
		t.Fatalf("expected regular file, got directory")
	}
}

func TestMultiClusterFile(t *testing.T) {
	size := int64(20 * 1024 * 1024)
	bk := createTestBackend(t, size)

	fsys, err := exfat.Create(bk, size, 0, 512, "BIGFILE")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	filePath := "/large.bin"
	f, err := fsys.OpenFile(filePath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}

	// 50 KiB (spans across multiple 4 KiB clusters)
	data := make([]byte, 50*1024)
	for i := range data {
		data[i] = byte(i % 251)
	}

	n, err := f.Write(data)
	if err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if n != len(data) {
		t.Fatalf("expected %d written, got %d", len(data), n)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	// Verify content
	readData, err := fsys.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if !bytes.Equal(readData, data) {
		t.Fatal("multi-cluster read data does not match written data")
	}

	// Verify cluster chain
	fRead, err := fsys.OpenFile(filePath, os.O_RDONLY)
	if err != nil {
		t.Fatalf("open read failed: %v", err)
	}
	defer fRead.Close()

	exFile := fRead.(*exfat.File)
	chain, err := exFile.GetClusterChain()
	if err != nil {
		t.Fatalf("get cluster chain failed: %v", err)
	}
	expectedClusters := (len(data) + 4096 - 1) / 4096
	if len(chain) != expectedClusters {
		t.Fatalf("expected %d clusters, got %d", expectedClusters, len(chain))
	}

	ranges, err := exFile.GetDiskRanges()
	if err != nil {
		t.Fatalf("get disk ranges failed: %v", err)
	}
	if len(ranges) == 0 {
		t.Fatal("expected at least one disk range")
	}
}

func TestDirectoryOperations(t *testing.T) {
	size := int64(20 * 1024 * 1024)
	bk := createTestBackend(t, size)

	fsys, err := exfat.Create(bk, size, 0, 512, "DIRS")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	// Mkdir
	if err := fsys.Mkdir("/sub1"); err != nil {
		t.Fatalf("mkdir /sub1 failed: %v", err)
	}
	if err := fsys.Mkdir("/sub1/nested"); err != nil {
		t.Fatalf("mkdir /sub1/nested failed: %v", err)
	}

	// Duplicate mkdir should fail
	if err := fsys.Mkdir("/sub1"); !errors.Is(err, os.ErrExist) {
		t.Fatalf("expected ErrExist on duplicate mkdir, got %v", err)
	}

	// Create file in nested directory
	filePath := "/sub1/nested/test.txt"
	f, err := fsys.OpenFile(filePath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("open file in nested dir failed: %v", err)
	}
	if _, err := f.Write([]byte("nested content")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	f.Close()

	// ReadDir root
	rootEntries, err := fsys.ReadDir("/")
	if err != nil {
		t.Fatalf("read root dir failed: %v", err)
	}
	if len(rootEntries) != 1 || rootEntries[0].Name() != "sub1" || !rootEntries[0].IsDir() {
		t.Fatalf("unexpected root entries: %+v", rootEntries)
	}

	// ReadDir nested
	nestedEntries, err := fsys.ReadDir("/sub1/nested")
	if err != nil {
		t.Fatalf("read nested dir failed: %v", err)
	}
	if len(nestedEntries) != 1 || nestedEntries[0].Name() != "test.txt" || nestedEntries[0].IsDir() {
		t.Fatalf("unexpected nested entries: %+v", nestedEntries)
	}

	// Rename file
	newFilePath := "/sub1/nested/renamed.txt"
	if err := fsys.Rename(filePath, newFilePath); err != nil {
		t.Fatalf("rename failed: %v", err)
	}

	// Verify old file gone and new file present
	if _, err := fsys.ReadFile(filePath); !errors.Is(err, os.ErrNotExist) && err == nil {
		t.Fatalf("expected old file to not exist after rename")
	}
	renamedContent, err := fsys.ReadFile(newFilePath)
	if err != nil {
		t.Fatalf("reading renamed file failed: %v", err)
	}
	if string(renamedContent) != "nested content" {
		t.Fatalf("unexpected content in renamed file: %s", string(renamedContent))
	}

	// Cannot remove non-empty directory
	if err := fsys.Remove("/sub1/nested"); err == nil {
		t.Fatal("expected error removing non-empty directory")
	}

	// Remove file then remove empty dir
	if err := fsys.Remove(newFilePath); err != nil {
		t.Fatalf("remove file failed: %v", err)
	}
	if err := fsys.Remove("/sub1/nested"); err != nil {
		t.Fatalf("remove empty nested dir failed: %v", err)
	}
	if err := fsys.Remove("/sub1"); err != nil {
		t.Fatalf("remove empty sub1 dir failed: %v", err)
	}
}

func TestSeekTruncateAppend(t *testing.T) {
	size := int64(20 * 1024 * 1024)
	bk := createTestBackend(t, size)

	fsys, err := exfat.Create(bk, size, 0, 512, "SEEK")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	filePath := "/seektest.txt"
	f, err := fsys.OpenFile(filePath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}

	f.Write([]byte("0123456789"))

	// Seek and overwrite
	pos, err := f.Seek(5, io.SeekStart)
	if err != nil || pos != 5 {
		t.Fatalf("seek failed: pos=%d, err=%v", pos, err)
	}
	f.Write([]byte("ABCDE"))
	f.Close()

	content, err := fsys.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(content) != "01234ABCDE" {
		t.Fatalf("unexpected content: %s", string(content))
	}

	// Append
	fAppend, err := fsys.OpenFile(filePath, os.O_APPEND|os.O_RDWR)
	if err != nil {
		t.Fatalf("open append failed: %v", err)
	}
	fAppend.Write([]byte("XYZ"))
	fAppend.Close()

	content, err = fsys.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(content) != "01234ABCDEXYZ" {
		t.Fatalf("unexpected appended content: %s", string(content))
	}

	// Truncate
	fTrunc, err := fsys.OpenFile(filePath, os.O_RDWR)
	if err != nil {
		t.Fatalf("open trunc failed: %v", err)
	}
	exTrunc := fTrunc.(*exfat.File)
	if err := exTrunc.Truncate(5); err != nil {
		t.Fatalf("truncate failed: %v", err)
	}
	fTrunc.Close()

	content, err = fsys.ReadFile(filePath)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	if string(content) != "01234" {
		t.Fatalf("unexpected truncated content: %s", string(content))
	}
}

func TestSetLabelAndChtimes(t *testing.T) {
	size := int64(20 * 1024 * 1024)
	bk := createTestBackend(t, size)

	fsys, err := exfat.Create(bk, size, 0, 512, "OLDLABEL")
	if err != nil {
		t.Fatalf("create failed: %v", err)
	}

	if err := fsys.SetLabel("NEWLABEL"); err != nil {
		t.Fatalf("set label failed: %v", err)
	}
	if fsys.Label() != "NEWLABEL" {
		t.Fatalf("expected NEWLABEL, got %s", fsys.Label())
	}

	filePath := "/timetest.txt"
	f, err := fsys.OpenFile(filePath, os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("open failed: %v", err)
	}
	f.Write([]byte("time"))
	f.Close()

	testTime := time.Date(2025, 5, 10, 14, 30, 20, 0, time.UTC)
	if err := fsys.Chtimes(filePath, testTime, testTime, testTime); err != nil {
		t.Fatalf("chtimes failed: %v", err)
	}

	info, err := fsys.Stat(filePath)
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if info.ModTime().Year() != 2025 || info.ModTime().Month() != 5 || info.ModTime().Day() != 10 {
		t.Fatalf("unexpected mod time: %v", info.ModTime())
	}
}

func TestDiskIntegration(t *testing.T) {
	diskPath := filepath.Join(t.TempDir(), "disk_exfat.img")
	size := int64(30 * 1024 * 1024)

	d, err := diskfs.Create(diskPath, size, diskfs.SectorSizeDefault)
	if err != nil {
		t.Fatalf("diskfs.Create failed: %v", err)
	}

	fsys, err := d.CreateFilesystem(disk.FilesystemSpec{
		Partition:   0,
		FSType:      filesystem.TypeExFAT,
		VolumeLabel: "INTEG",
	})
	if err != nil {
		t.Fatalf("CreateFilesystem failed: %v", err)
	}

	if fsys.Type() != filesystem.TypeExFAT {
		t.Fatalf("expected TypeExFAT, got %v", fsys.Type())
	}

	// Create file through disk's filesystem
	f, err := fsys.OpenFile("/diskfile.txt", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatalf("open file failed: %v", err)
	}
	f.Write([]byte("written via disk.Disk"))
	f.Close()

	// Reopen disk and get filesystem
	d2, err := diskfs.Open(diskPath)
	if err != nil {
		t.Fatalf("diskfs.Open failed: %v", err)
	}

	fsys2, err := d2.GetFilesystem(0)
	if err != nil {
		t.Fatalf("GetFilesystem failed: %v", err)
	}

	if fsys2.Type() != filesystem.TypeExFAT {
		t.Fatalf("expected TypeExFAT from GetFilesystem, got %v", fsys2.Type())
	}

	content, err := fsys2.ReadFile("/diskfile.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(content) != "written via disk.Disk" {
		t.Fatalf("unexpected content: %s", string(content))
	}
}
