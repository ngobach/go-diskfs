package exfat

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// exFatHash calculates the 16-bit hash of an up-cased Unicode character,
// matching SdFat / ExFatName.cpp: exFatHash.
func exFatHash(u uint16, hash uint16) uint16 {
	c := toUpcase(u)
	hash = ((hash << 15) | (hash >> 1)) + (c & 0xFF)
	hash = ((hash << 15) | (hash >> 1)) + (c >> 8)
	return hash
}

// HashName computes the 16-bit name hash for a UTF-16 filename,
// matching SdFat / ExFatFile::hashName.
func HashName(name []uint16) (uint16, error) {
	if len(name) == 0 || len(name) > ExFatMaxNameLength {
		return 0, fmt.Errorf("invalid filename length: %d (max %d)", len(name), ExFatMaxNameLength)
	}
	var hash uint16
	for _, u := range name {
		hash = exFatHash(u, hash)
	}
	return hash, nil
}

// CmpName compares two UTF-16 strings case-insensitively using toUpcase,
// matching SdFat / ExFatFile::cmpName.
func CmpName(a, b []uint16) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if toUpcase(a[i]) != toUpcase(b[i]) {
			return false
		}
	}
	return true
}

// StringToUTF16 converts a UTF-8 string to a slice of UTF-16 code units.
func StringToUTF16(s string) []uint16 {
	return utf16.Encode([]rune(s))
}

// UTF16ToString converts a slice of UTF-16 code units (up to null terminator) to a UTF-8 string.
func UTF16ToString(u []uint16) string {
	for i, c := range u {
		if c == 0 {
			u = u[:i]
			break
		}
	}
	return string(utf16.Decode(u))
}

// ValidateFilename checks if a filename is valid for an exFAT file or directory.
func ValidateFilename(name string) error {
	if name == "" {
		return fmt.Errorf("filename cannot be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("filename cannot be . or ..")
	}
	u := StringToUTF16(name)
	if len(u) > ExFatMaxNameLength {
		return fmt.Errorf("filename exceeds maximum length of %d characters", ExFatMaxNameLength)
	}
	for _, r := range name {
		if r < 0x20 {
			return fmt.Errorf("filename contains invalid control character: 0x%02X", r)
		}
		switch r {
		case '"', '*', '/', ':', '<', '>', '?', '\\', '|':
			return fmt.Errorf("filename contains reserved character %q", r)
		}
	}
	return nil
}

// SplitPath cleans and splits a path into components.
func SplitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" || p == "." {
		return nil
	}
	parts := strings.Split(p, "/")
	var res []string
	for _, part := range parts {
		if part != "" && part != "." {
			res = append(res, part)
		}
	}
	return res
}
