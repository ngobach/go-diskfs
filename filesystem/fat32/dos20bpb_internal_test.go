package fat32

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/ngobach/go-diskfs/filesystem/fat12"
)

func getValidDos20BPB() *fat12.Dos20BPB {
	return &fat12.Dos20BPB{
		BytesPerSector:       512,
		SectorsPerCluster:    1,
		ReservedSectors:      32,
		FatCount:             2,
		RootDirectoryEntries: 0,
		TotalSectors:         0x5000,
		MediaType:            0xf8,
		SectorsPerFat:        0,
	}
}

func TestDos20BPBFromBytes(t *testing.T) {
	t.Run("mismatched length", func(t *testing.T) {
		b := make([]byte, 12, 13)
		bpb, err := fat12.Dos20BPBFromBytes(b)
		if err == nil {
			t.Errorf("Did not return expected error")
		}
		if bpb != nil {
			t.Fatalf("returned bpb was non-nil")
		}
		expected := "cannot read DOS 2.0 BPB"
		if !strings.HasPrefix(err.Error(), expected) {
			t.Errorf("error type %s instead of expected %s", err.Error(), expected)
		}
	})
	t.Run("invalid sector size", func(t *testing.T) {
		size := uint16(511)
		b := make([]byte, 13)
		binary.LittleEndian.PutUint16(b[0:2], size)
		bpb, err := fat12.Dos20BPBFromBytes(b)
		if err == nil {
			t.Errorf("Did not return expected error")
		}
		if bpb != nil {
			t.Fatalf("returned bpb was non-nil")
		}
		expected := fmt.Sprintf("invalid sector size %d", size)
		if !strings.HasPrefix(err.Error(), expected) {
			t.Errorf("error type %s instead of expected %s", err.Error(), expected)
		}
	})
	t.Run("valid data", func(t *testing.T) {
		input, err := os.ReadFile(Fat32File)
		if err != nil {
			t.Fatalf("error reading test fixture data from %s: %v", Fat32File, err)
		}
		inputBytes := input[11:24]
		bpb, err := fat12.Dos20BPBFromBytes(inputBytes)
		if err != nil {
			t.Errorf("returned unexpected non-nil error: %v", err)
		}
		if bpb == nil {
			t.Fatalf("returned bpb was nil")
		}
		valid := getValidDos20BPB()
		if *bpb != *valid {
			t.Log(bpb)
			t.Log(valid)
			t.Fatalf("Mismatched BPB")
		}
	})
}

func TestDos20BPBToBytes(t *testing.T) {
	bpb := getValidDos20BPB()
	b := bpb.ToBytes()
	if b == nil {
		t.Fatal("b was nil unexpectedly")
	}
	valid, err := os.ReadFile(Fat32File)
	if err != nil {
		t.Fatalf("error reading test fixture data from %s: %v", Fat32File, err)
	}
	validBytes := valid[11:24]
	if !bytes.Equal(validBytes, b) {
		t.Log(validBytes)
		t.Log(b)
		t.Error("Mismatched bytes")
	}
}
