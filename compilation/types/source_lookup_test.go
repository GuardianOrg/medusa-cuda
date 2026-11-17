package types

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInstructionIndexAtPC checks that PUSH data is skipped when counting instructions.
func TestInstructionIndexAtPC(t *testing.T) {
	// PUSH1 0x80, PUSH2 0x0102, ADD, PUSH32 <32 bytes>, STOP
	bytecode := []byte{0x60, 0x80, 0x61, 0x01, 0x02, 0x01, 0x7f}
	bytecode = append(bytecode, make([]byte, 32)...)
	bytecode = append(bytecode, 0x00)

	for pc, want := range map[uint64]int{0: 0, 2: 1, 5: 2, 6: 3, 39: 4} {
		index, err := InstructionIndexAtPC(bytecode, pc)
		require.NoError(t, err, "pc %d", pc)
		assert.Equal(t, want, index, "pc %d", pc)
	}
	for _, pc := range []uint64{1, 3, 4, 7, 38, 40, 1000} {
		_, err := InstructionIndexAtPC(bytecode, pc)
		assert.Error(t, err, "pc %d", pc)
	}
}

// TestSourceRangeAtPC checks the source range, lines and fragment found for program counters, and generated sources.
func TestSourceRangeAtPC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "A.sol")
	code := "contract A {\n  function f(uint a) external returns (uint) {\n    unchecked { return a + 1; }\n  }\n}\n"
	require.NoError(t, os.WriteFile(path, []byte(code), 0o644))
	addOffset := len("contract A {\n  function f(uint a) external returns (uint) {\n    unchecked { return ")

	compilation := NewCompilation()
	compilation.SourceIdToPath[0] = path
	contract := &CompiledContract{
		// PUSH1 1, ADD, ADD, STOP
		RuntimeBytecode: []byte{0x60, 0x01, 0x01, 0x01, 0x00},
		// PUSH1 maps to the whole contract, the first ADD to `a + 1`, the second to generated source 1, STOP to none.
		SrcMapsRuntime: "0:" + strconv.Itoa(len(code)-1) + ":0;" + strconv.Itoa(addOffset) + ":5:0;3:4:1;-1:-1:-1",
	}

	whole, err := compilation.SourceRangeAtPC(contract, 0, true)
	require.NoError(t, err)
	assert.Equal(t, 1, whole.StartLine)
	assert.Equal(t, 5, whole.EndLine)

	add, err := compilation.SourceRangeAtPC(contract, 2, true)
	require.NoError(t, err)
	assert.Equal(t, "a + 1", add.Fragment)
	assert.Equal(t, path, add.SourcePath)
	assert.Equal(t, 3, add.StartLine)
	assert.Equal(t, 3, add.EndLine)
	assert.False(t, add.IsGenerated())
	wrapping, err := compilation.InWrappingBlock(add)
	require.NoError(t, err)
	assert.True(t, wrapping)

	generated, err := compilation.SourceRangeAtPC(contract, 3, true)
	require.NoError(t, err)
	assert.True(t, generated.IsGenerated())
	assert.Equal(t, 1, generated.SourceUnitID)
	assert.Empty(t, generated.Fragment)

	_, err = compilation.SourceRangeAtPC(contract, 4, true)
	assert.ErrorIs(t, err, ErrNoSourceForPC)

	// The source code cache is used before the file on disk.
	compilation.SourceCode[path] = []byte("x")
	_, err = compilation.SourceRangeAtPC(contract, 2, true)
	assert.ErrorContains(t, err, "past the end")
}

// TestCompilerFromMetadata checks solc's and Solar's metadata, and bytecode without metadata.
func TestCompilerFromMetadata(t *testing.T) {
	// solc 0.8.38: {"ipfs": <34 bytes>, "solc": 0x000826}
	solc, _ := hex.DecodeString("a2646970667358221220" + strings.Repeat("ab", 32) + "64736f6c63430008260033")
	compiler, ok := CompilerFromMetadata(append([]byte{0x60, 0x80}, solc...))
	require.True(t, ok)
	assert.Equal(t, BytecodeCompiler{Name: "solc", Major: 0, Minor: 8, Patch: 38}, compiler)
	assert.True(t, compiler.ChecksArithmetic())

	// solc 0.7.6 does not check arithmetic.
	old, _ := hex.DecodeString("a2646970667358221220" + strings.Repeat("ab", 32) + "64736f6c63430007060033")
	compiler, ok = CompilerFromMetadata(old)
	require.True(t, ok)
	assert.False(t, compiler.ChecksArithmetic())

	// Solar writes its own version as text, after an "experimental" flag here.
	version := "0.3.0-dev+commit.597c60d0"
	solar := "a3" + "6c6578706572696d656e74616c" + "f5" + "65736f6c6172" + "78" + hex.EncodeToString([]byte{byte(len(version))}) +
		hex.EncodeToString([]byte(version)) + "646970667340"
	solarBytes, _ := hex.DecodeString(solar)
	solarBytes = append(solarBytes, 0x00, byte(len(solarBytes)))
	compiler, ok = CompilerFromMetadata(solarBytes)
	require.True(t, ok)
	assert.Equal(t, BytecodeCompiler{Name: "solar", Major: 0, Minor: 3, Patch: 0}, compiler)
	assert.True(t, compiler.ChecksArithmetic())

	// A solc prerelease writes its version as text.
	prerelease := "a1" + "64736f6c63" + "78" + hex.EncodeToString([]byte{byte(len("0.8.39-nightly.2026.11.1"))}) +
		hex.EncodeToString([]byte("0.8.39-nightly.2026.11.1"))
	prereleaseBytes, _ := hex.DecodeString(prerelease)
	prereleaseBytes = append(prereleaseBytes, 0x00, byte(len(prereleaseBytes)))
	compiler, ok = CompilerFromMetadata(prereleaseBytes)
	require.True(t, ok)
	assert.Equal(t, BytecodeCompiler{Name: "solc", Major: 0, Minor: 8, Patch: 39}, compiler)

	for _, bytecode := range [][]byte{nil, {0x00}, {0x60, 0x80, 0x52, 0x00}, {0xff, 0x00, 0x01}} {
		_, ok := CompilerFromMetadata(bytecode)
		assert.False(t, ok, "%x", bytecode)
	}
}

// TestWrappingBlockSpans checks which blocks are found, with comments and strings in the way.
func TestWrappingBlockSpans(t *testing.T) {
	code := `contract C {
    // unchecked { not code }
    /* assembly { not code } */
    string s = "unchecked { }";
    function f(uint a) external returns (uint b) {
        unchecked { if (a > 0) { b = a + 1; } }
        b = a + 2;
        assembly ("memory-safe") { b := add(b, 3) }
        assembly { b := mul(b, "}") }
    }
}`
	spans := WrappingBlockSpans([]byte(code))
	var blocks []string
	for _, span := range spans {
		blocks = append(blocks, code[span[0]:span[1]])
	}
	assert.Equal(t, []string{
		"{ if (a > 0) { b = a + 1; } }",
		`{ b := add(b, 3) }`,
		`{ b := mul(b, "}") }`,
	}, blocks)

	inside := func(fragment string) bool {
		for _, span := range spans {
			offset := strings.Index(code, fragment)
			if offset >= span[0] && offset+len(fragment) <= span[1] {
				return true
			}
		}
		return false
	}
	assert.True(t, inside("a + 1"))
	assert.False(t, inside("a + 2"))
}
