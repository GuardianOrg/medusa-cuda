package types

import (
	"encoding/binary"
	"strconv"
	"strings"
)

// BytecodeCompiler describes the compiler named in the CBOR metadata that solc and Solar append to bytecode.
type BytecodeCompiler struct {
	// Name is "solc" or "solar".
	Name string

	// Major, Minor and Patch are the compiler's own version: the Solidity version for solc, and Solar's version
	// (such as 0.3.0) for Solar.
	Major, Minor, Patch int
}

// ChecksArithmetic reports whether the compiler reverts on arithmetic overflow outside `unchecked` blocks, as
// Solidity does from 0.8 on. Solar only compiles Solidity 0.8 and later.
func (c BytecodeCompiler) ChecksArithmetic() bool {
	return c.Name == "solar" || c.AtLeast(0, 8, 0)
}

// AtLeast reports whether the compiler's own version is at least the given one.
func (c BytecodeCompiler) AtLeast(major, minor, patch int) bool {
	if c.Major != major {
		return c.Major > major
	}
	if c.Minor != minor {
		return c.Minor > minor
	}
	return c.Patch >= patch
}

// CompilerFromMetadata reads the compiler from the CBOR metadata at the end of a contract's bytecode. solc writes
// its version under the key "solc" (three bytes for a release, text for a prerelease), and Solar writes its own
// version as text such as "0.3.0-dev+commit.597c60d0" under the key "solar". It returns false when the bytecode carries no such metadata, as
// when it was compiled with the metadata turned off.
func CompilerFromMetadata(bytecode []byte) (BytecodeCompiler, bool) {
	if len(bytecode) < 2 {
		return BytecodeCompiler{}, false
	}
	length := int(binary.BigEndian.Uint16(bytecode[len(bytecode)-2:]))
	if length == 0 || length > len(bytecode)-2 {
		return BytecodeCompiler{}, false
	}
	entries, ok := parseCborMap(bytecode[len(bytecode)-2-length : len(bytecode)-2])
	if !ok {
		return BytecodeCompiler{}, false
	}

	for _, name := range []string{"solar", "solc"} {
		value, found := entries[name]
		if !found {
			continue
		}
		compiler := BytecodeCompiler{Name: name}
		if value.isBytes && len(value.data) == 3 {
			compiler.Major, compiler.Minor, compiler.Patch = int(value.data[0]), int(value.data[1]), int(value.data[2])
			return compiler, true
		}
		if !value.isBytes {
			if compiler.Major, compiler.Minor, compiler.Patch, ok = parseVersion(string(value.data)); ok {
				return compiler, true
			}
		}
	}
	return BytecodeCompiler{}, false
}

// parseVersion reads the "major.minor.patch" at the start of a version string such as "0.8.38+commit.a2b44354".
func parseVersion(version string) (int, int, int, bool) {
	if end := strings.IndexAny(version, "+-"); end >= 0 {
		version = version[:end]
	}
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var numbers [3]int
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return 0, 0, 0, false
		}
		numbers[i] = n
	}
	return numbers[0], numbers[1], numbers[2], true
}

// cborValue is a byte string or text string value of a CBOR map, or a value of another type with no data.
type cborValue struct {
	isBytes bool
	data    []byte
}

// parseCborMap reads a CBOR map with text keys, as compilers write their metadata. Byte string and text values are
// kept; booleans are skipped. It returns false for anything else.
func parseCborMap(data []byte) (map[string]cborValue, bool) {
	pos := 0
	readHead := func() (major byte, argument int, ok bool) {
		if pos >= len(data) {
			return 0, 0, false
		}
		head := data[pos]
		pos++
		major, info := head>>5, int(head&0x1f)
		switch {
		case info < 24:
			return major, info, true
		case info == 24 && pos+1 <= len(data):
			pos++
			return major, int(data[pos-1]), true
		case info == 25 && pos+2 <= len(data):
			pos += 2
			return major, int(binary.BigEndian.Uint16(data[pos-2:])), true
		}
		return 0, 0, false
	}
	readData := func(length int) ([]byte, bool) {
		if pos+length > len(data) {
			return nil, false
		}
		pos += length
		return data[pos-length : pos], true
	}

	major, count, ok := readHead()
	if !ok || major != 5 {
		return nil, false
	}
	entries := make(map[string]cborValue, count)
	for i := 0; i < count; i++ {
		major, length, ok := readHead()
		if !ok || major != 3 {
			return nil, false
		}
		key, ok := readData(length)
		if !ok {
			return nil, false
		}

		major, argument, ok := readHead()
		if !ok {
			return nil, false
		}
		switch major {
		case 2, 3:
			value, ok := readData(argument)
			if !ok {
				return nil, false
			}
			entries[string(key)] = cborValue{isBytes: major == 2, data: value}
		case 7:
			// A simple value such as true or false ("experimental").
		default:
			return nil, false
		}
	}
	return entries, pos == len(data)
}
