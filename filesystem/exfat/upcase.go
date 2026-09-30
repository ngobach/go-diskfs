package exfat

// Ported from SdFat / src/common/upcase.cpp and upcase.h

type map16 struct {
	base  uint16
	off   int8
	count uint8
}

type pair16 struct {
	key uint16
	val uint16
}

var mapTable = []map16{
	{0x0061, -32, 26}, {0x00E0, -32, 23}, {0x00F8, -32, 7}, {0x0100, 1, 48},
	{0x0132, 1, 6}, {0x0139, 1, 16}, {0x014A, 1, 46}, {0x0179, 1, 6},
	{0x0182, 1, 4}, {0x01A0, 1, 6}, {0x01B3, 1, 4}, {0x01CD, 1, 16},
	{0x01DE, 1, 18}, {0x01F8, 1, 40}, {0x0222, 1, 18}, {0x0246, 1, 10},
	{0x03AD, -37, 3}, {0x03B1, -32, 17}, {0x03C3, -32, 9}, {0x03D8, 1, 24},
	{0x0430, -32, 32}, {0x0450, -80, 16}, {0x0460, 1, 34}, {0x048A, 1, 54},
	{0x04C1, 1, 14}, {0x04D0, 1, 68}, {0x0561, -48, 38}, {0x1E00, 1, 150},
	{0x1EA0, 1, 90}, {0x1F00, 8, 8}, {0x1F10, 8, 6}, {0x1F20, 8, 8},
	{0x1F30, 8, 8}, {0x1F40, 8, 6}, {0x1F60, 8, 8}, {0x1F70, 74, 2},
	{0x1F72, 86, 4}, {0x1F76, 100, 2}, {0x1F7A, 112, 2}, {0x1F7C, 126, 2},
	{0x1F80, 8, 8}, {0x1F90, 8, 8}, {0x1FA0, 8, 8}, {0x1FB0, 8, 2},
	{0x1FD0, 8, 2}, {0x1FE0, 8, 2}, {0x2170, -16, 16}, {0x24D0, -26, 26},
	{0x2C30, -48, 47}, {0x2C67, 1, 6}, {0x2C80, 1, 100}, {0x2D00, 0, 38},
	{0xFF41, -32, 26},
}

var lookupTable = []pair16{
	{0x00FF, 0x0178}, {0x0180, 0x0243}, {0x0188, 0x0187}, {0x018C, 0x018B},
	{0x0192, 0x0191}, {0x0195, 0x01F6}, {0x0199, 0x0198}, {0x019A, 0x023D},
	{0x019E, 0x0220}, {0x01A8, 0x01A7}, {0x01AD, 0x01AC}, {0x01B0, 0x01AF},
	{0x01B9, 0x01B8}, {0x01BD, 0x01BC}, {0x01BF, 0x01F7}, {0x01C6, 0x01C4},
	{0x01C9, 0x01C7}, {0x01CC, 0x01CA}, {0x01DD, 0x018E}, {0x01F3, 0x01F1},
	{0x01F5, 0x01F4}, {0x023A, 0x2C65}, {0x023C, 0x023B}, {0x023E, 0x2C66},
	{0x0242, 0x0241}, {0x0253, 0x0181}, {0x0254, 0x0186}, {0x0256, 0x0189},
	{0x0257, 0x018A}, {0x0259, 0x018F}, {0x025B, 0x0190}, {0x0260, 0x0193},
	{0x0263, 0x0194}, {0x0268, 0x0197}, {0x0269, 0x0196}, {0x026B, 0x2C62},
	{0x026F, 0x019C}, {0x0272, 0x019D}, {0x0275, 0x019F}, {0x027D, 0x2C64},
	{0x0280, 0x01A6}, {0x0283, 0x01A9}, {0x0288, 0x01AE}, {0x0289, 0x0244},
	{0x028A, 0x01B1}, {0x028B, 0x01B2}, {0x028C, 0x0245}, {0x0292, 0x01B7},
	{0x037B, 0x03FD}, {0x037C, 0x03FE}, {0x037D, 0x03FF}, {0x03AC, 0x0386},
	{0x03C2, 0x03A3}, {0x03CC, 0x038C}, {0x03CD, 0x038E}, {0x03CE, 0x038F},
	{0x03F2, 0x03F9}, {0x03F8, 0x03F7}, {0x03FB, 0x03FA}, {0x04CF, 0x04C0},
	{0x1D7D, 0x2C63}, {0x1F51, 0x1F59}, {0x1F53, 0x1F5B}, {0x1F55, 0x1F5D},
	{0x1F57, 0x1F5F}, {0x1F78, 0x1FF8}, {0x1F79, 0x1FF9}, {0x1FB3, 0x1FBC},
	{0x1FCC, 0x1FC3}, {0x1FE5, 0x1FEC}, {0x1FFC, 0x1FF3}, {0x214E, 0x2132},
	{0x2184, 0x2183}, {0x2C61, 0x2C60}, {0x2C76, 0x2C75},
}

func searchMapTable(key uint16) int {
	left := 0
	right := len(mapTable)
	for right-left > 1 {
		mid := left + (right-left)/2
		if mapTable[mid].base <= key {
			left = mid
		} else {
			right = mid
		}
	}
	return left
}

func searchLookupTable(key uint16) int {
	left := 0
	right := len(lookupTable)
	for right-left > 1 {
		mid := left + (right-left)/2
		if lookupTable[mid].key <= key {
			left = mid
		} else {
			right = mid
		}
	}
	return left
}

// toUpcase converts a 16-bit Unicode character to its uppercase equivalent according to exFAT spec.
func toUpcase(chr uint16) uint16 {
	if chr < 127 {
		if 'a' <= chr && chr <= 'z' {
			return chr - ('a' - 'A')
		}
		return chr
	}
	i := searchMapTable(chr)
	first := mapTable[i].base
	if first <= chr && uint32(chr-first) < uint32(mapTable[i].count) {
		off := mapTable[i].off
		if off == 1 {
			return chr - ((chr - first) & 1)
		}
		if off != 0 {
			return uint16(int32(chr) + int32(off))
		}
		return uint16(int32(chr) - 0x1C60)
	}
	j := searchLookupTable(chr)
	if lookupTable[j].key == chr {
		return lookupTable[j].val
	}
	return chr
}

// upcaseChecksum updates the 32-bit checksum for an upcase Unicode character.
func upcaseChecksum(uc uint16, sum uint32) uint32 {
	sum = (sum << 31) + (sum >> 1) + uint32(uc&0xFF)
	sum = (sum << 31) + (sum >> 1) + uint32(uc>>8)
	return sum
}

const MinimumUpcaseSkip = 512

// GenerateUpcaseTable generates the compressed up-case table bytes and its 32-bit checksum
// matching SdFat's ExFatFormatter::writeUpcase.
func GenerateUpcaseTable() ([]byte, uint32) {
	var table []byte
	var checksum uint32

	writeUnicode := func(u uint16) {
		b0 := byte(u & 0xFF)
		b1 := byte(u >> 8)
		table = append(table, b0, b1)
		checksum = upcaseChecksum(u, checksum)
	}

	var ch uint32
	for ch < 0x10000 {
		uc := toUpcase(uint16(ch))
		if uc != uint16(ch) {
			writeUnicode(uc)
			ch++
		} else {
			n := ch + 1
			for n < 0x10000 && n == uint32(toUpcase(uint16(n))) {
				n++
			}
			ns := n - ch
			if ns >= MinimumUpcaseSkip {
				// write compressed run: 0xFFFF followed by count
				writeUnicode(0xFFFF)
				writeUnicode(uint16(ns))
				ch = n
			} else {
				for ch < n {
					writeUnicode(uint16(ch))
					ch++
				}
			}
		}
	}

	return table, checksum
}
