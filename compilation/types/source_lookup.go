package types

import (
	"errors"
	"fmt"
	"os"
	"sort"
)

// SourceRange describes the source code a compiler's source map gives for one instruction.
type SourceRange struct {
	// SourcePath is the path of the source file. It is empty when the range is in a source the compiler generated,
	// such as solc's Yul utility functions, which are not part of the compilation's sources.
	SourcePath string

	// SourceUnitID is the compiler's id of the source file.
	SourceUnitID int

	// Offset is the byte offset of the range in the source file.
	Offset int

	// Length is the length of the range in bytes.
	Length int

	// StartLine is the 1-based line number the range starts on. It is 0 for a generated source.
	StartLine int

	// EndLine is the 1-based line number the range ends on. It is 0 for a generated source.
	EndLine int

	// Fragment is the source code of the range. It is empty for a generated source.
	Fragment string
}

// IsGenerated reports whether the range is in a source the compiler generated rather than in one of the
// compilation's source files.
func (r *SourceRange) IsGenerated() bool {
	return r.SourcePath == ""
}

// ErrNoSourceForPC is returned when the source map gives no source for an instruction, as for compiler-inserted code.
var ErrNoSourceForPC = errors.New("the source map gives no source for this instruction")

// SourceRangeAtPC returns the source code that the contract's source map gives for the instruction at a program
// counter (a byte offset into the contract's runtime bytecode, or into its init bytecode when runtime is false).
//
// The range comes from the source map of the compiler that built this compilation, so it is right for solc and Solar
// alike: the two compilers emit different bytecode, so a program counter can only be resolved with the source map of
// the bytecode it was taken from.
func (c *Compilation) SourceRangeAtPC(contract *CompiledContract, pc uint64, runtime bool) (*SourceRange, error) {
	bytecode, sourceMapStr := contract.InitBytecode, contract.SrcMapsInit
	if runtime {
		bytecode, sourceMapStr = contract.RuntimeBytecode, contract.SrcMapsRuntime
	}

	index, err := InstructionIndexAtPC(bytecode, pc)
	if err != nil {
		return nil, err
	}
	sourceMap, err := ParseSourceMap(sourceMapStr)
	if err != nil {
		return nil, fmt.Errorf("could not parse the source map: %w", err)
	}
	if index >= len(sourceMap) {
		return nil, fmt.Errorf("pc %d is instruction %d, past the %d instructions the source map covers", pc, index,
			len(sourceMap))
	}

	element := sourceMap[index]
	if element.SourceUnitID < 0 || element.Offset < 0 || element.Length < 0 {
		return nil, ErrNoSourceForPC
	}
	sourceRange := &SourceRange{
		SourceUnitID: element.SourceUnitID,
		Offset:       element.Offset,
		Length:       element.Length,
	}

	sourcePath, ok := c.SourceIdToPath[element.SourceUnitID]
	if !ok {
		// The id names a source the compiler generated.
		return sourceRange, nil
	}
	sourceRange.SourcePath = sourcePath

	code, err := c.sourceCode(sourcePath)
	if err != nil {
		return nil, err
	}
	end := element.Offset + element.Length
	if end > len(code) {
		return nil, fmt.Errorf("source range %d:%d is past the end of '%s' (%d bytes)", element.Offset,
			element.Length, sourcePath, len(code))
	}
	sourceRange.Fragment = string(code[element.Offset:end])
	sourceRange.StartLine = lineAtOffset(code, element.Offset)
	sourceRange.EndLine = sourceRange.StartLine
	if element.Length > 0 {
		sourceRange.EndLine = lineAtOffset(code, end-1)
	}
	return sourceRange, nil
}

// sourceCode returns the source code of a file of the compilation, from the cache filled by CacheSourceCode, or else
// from disk.
func (c *Compilation) sourceCode(sourcePath string) ([]byte, error) {
	if code, ok := c.SourceCode[sourcePath]; ok && code != nil {
		return code, nil
	}
	code, err := os.ReadFile(sourcePath)
	if err != nil {
		return nil, fmt.Errorf("could not read source file '%s': %w", sourcePath, err)
	}
	return code, nil
}

// lineAtOffset returns the 1-based number of the line holding the byte at an offset.
func lineAtOffset(code []byte, offset int) int {
	line := 1
	for _, b := range code[:offset] {
		if b == '\n' {
			line++
		}
	}
	return line
}

// InstructionIndexAtPC returns the index of the instruction that starts at a program counter, which is the index of
// its element in the source map. It returns an error when the program counter is past the bytecode or inside the
// data of a PUSH instruction.
func InstructionIndexAtPC(bytecode []byte, pc uint64) (int, error) {
	offsets := instructionOffsets(bytecode)
	index := sort.Search(len(offsets), func(i int) bool { return uint64(offsets[i]) >= pc })
	if index == len(offsets) || uint64(offsets[index]) != pc {
		return 0, fmt.Errorf("pc %d is not the start of an instruction in %d bytes of bytecode", pc, len(bytecode))
	}
	return index, nil
}

// instructionOffsets returns the byte offset of each instruction in the bytecode. PUSH1 to PUSH32 (0x60 to 0x7f)
// are followed by 1 to 32 bytes of data.
func instructionOffsets(bytecode []byte) []int {
	offsets := make([]int, 0, len(bytecode))
	for pc := 0; pc < len(bytecode); pc++ {
		offsets = append(offsets, pc)
		if op := bytecode[pc]; op >= 0x60 && op <= 0x7f {
			pc += int(op-0x60) + 1
		}
	}
	return offsets
}
