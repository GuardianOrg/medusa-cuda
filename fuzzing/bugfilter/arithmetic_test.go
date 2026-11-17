package bugfilter

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crytic/medusa/compilation/platforms"
	"github.com/crytic/medusa/compilation/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testdata/arith-solc and testdata/arith-solar hold src/Arith.sol built by Foundry with solc 0.8.38 and with Solar
// main. Arith does the same additions, subtractions and multiplications checked (lines 8, 20 and 21), unchecked
// (lines 13 to 15) and in inline assembly (line 26).

// loadArith reads the Arith contract of a fixture build.
func loadArith(t *testing.T, compiler string) (*types.Compilation, *types.CompiledContract) {
	t.Helper()
	compilations, err := platforms.ReadCryticExports(filepath.Join("testdata", "arith-"+compiler, "export"), "", nil)
	require.NoError(t, err)
	require.Len(t, compilations, 1)
	compilation := &compilations[0]
	require.NoError(t, compilation.CacheSourceCode())
	for _, source := range compilation.SourcePathToArtifact {
		if contract, ok := source.Contracts["Arith"]; ok {
			return compilation, &contract
		}
	}
	t.Fatalf("Arith not found in the %s build", compiler)
	return nil, nil
}

// arithmeticPCs returns the program counter of every ADD, SUB and MUL in the bytecode, as CuEVM would report a bug
// at each of them.
func arithmeticPCs(bytecode []byte) map[uint64]ArithmeticOp {
	ops := map[byte]ArithmeticOp{0x01: Add, 0x02: Mul, 0x03: Sub}
	pcs := make(map[uint64]ArithmeticOp)
	for pc := 0; pc < len(bytecode); pc++ {
		if op, ok := ops[bytecode[pc]]; ok {
			pcs[uint64(pc)] = op
		}
		if op := bytecode[pc]; op >= 0x60 && op <= 0x7f {
			pc += int(op-0x60) + 1
		}
	}
	return pcs
}

// keptBugs returns the bugs that are not false positives, among bugs reported at every arithmetic instruction, as
// "<op> line <n>: <fragment>".
func keptBugs(compilation *types.Compilation, contract *types.CompiledContract) []string {
	var kept []string
	for pc, op := range arithmeticPCs(contract.RuntimeBytecode) {
		verdict := CheckArithmeticBug(compilation, contract, pc, op)
		if !verdict.FalsePositive {
			kept = append(kept, fmt.Sprintf("%s line %d: %s", op, verdict.Source.StartLine, verdict.Source.Fragment))
		}
	}
	return kept
}

// TestArithmeticBugsKeptForEachCompiler checks that only the unchecked arithmetic is kept, for solc and Solar.
func TestArithmeticBugsKeptForEachCompiler(t *testing.T) {
	want := []string{"ADD line 13: x + a", "SUB line 14: x - a", "MUL line 15: x * a"}
	for _, compiler := range []string{"solc", "solar"} {
		t.Run(compiler, func(t *testing.T) {
			compilation, contract := loadArith(t, compiler)
			assert.ElementsMatch(t, want, keptBugs(compilation, contract))
		})
	}
}

// TestSolarCheckedArithmeticIsFalsePositive checks that Solar's checked arithmetic, which its source map gives to
// the expression itself rather than to a generated helper as solc does, is a false positive.
func TestSolarCheckedArithmeticIsFalsePositive(t *testing.T) {
	compilation, contract := loadArith(t, "solar")
	checkedLines := map[int]bool{8: true, 20: true, 21: true}
	found := 0
	for pc, op := range arithmeticPCs(contract.RuntimeBytecode) {
		verdict := CheckArithmeticBug(compilation, contract, pc, op)
		if verdict.Source != nil && checkedLines[verdict.Source.StartLine] {
			found++
			assert.True(t, verdict.FalsePositive, "pc %d", pc)
			assert.True(t, strings.HasPrefix(verdict.Reason, "checked arithmetic (solar"), verdict.Reason)
		}
	}
	assert.Equal(t, 3, found, "Solar should map one ADD, SUB and MUL to the checked lines")
}

// TestBugPCsNeedTheirOwnCompiler checks why bugs can't be resolved by compiling again with solc: the program counters
// of Solar's unchecked arithmetic map to other code in solc's bytecode.
func TestBugPCsNeedTheirOwnCompiler(t *testing.T) {
	solarCompilation, solarContract := loadArith(t, "solar")
	solcCompilation, solcContract := loadArith(t, "solc")
	for pc, op := range arithmeticPCs(solarContract.RuntimeBytecode) {
		solar := CheckArithmeticBug(solarCompilation, solarContract, pc, op)
		if solar.FalsePositive {
			continue
		}
		solc, err := solcCompilation.SourceRangeAtPC(solcContract, pc, true)
		if err == nil {
			assert.NotEqual(t, solar.Source.Offset, solc.Offset, "pc %d maps to the same code in both builds", pc)
		}
	}
}

// TestArithmeticBugWithoutSource checks the false positives that have no source to check.
func TestArithmeticBugWithoutSource(t *testing.T) {
	compilation, contract := loadArith(t, "solar")
	verdict := CheckArithmeticBug(compilation, contract, 0, Add)
	assert.True(t, verdict.FalsePositive)
	assert.Nil(t, verdict.Source)

	// The byte after a PUSH1 is data, not an instruction.
	pushPC := -1
	for pc := 0; pc < len(contract.RuntimeBytecode) && pushPC < 0; pc++ {
		op := contract.RuntimeBytecode[pc]
		if op == 0x60 {
			pushPC = pc
		} else if op > 0x60 && op <= 0x7f {
			pc += int(op-0x60) + 1
		}
	}
	require.GreaterOrEqual(t, pushPC, 0)
	verdict = CheckArithmeticBug(compilation, contract, uint64(pushPC+1), Add)
	assert.True(t, verdict.FalsePositive)
	assert.Contains(t, verdict.Reason, "not the start of an instruction")

	verdict = CheckArithmeticBug(compilation, contract, uint64(len(contract.RuntimeBytecode)+10), Add)
	assert.True(t, verdict.FalsePositive)
}

// TestHasOperator checks the operator rules ported from solidityutils/filter_fp.py.
func TestHasOperator(t *testing.T) {
	assert.True(t, hasOperator("x + a", Add))
	assert.False(t, hasOperator("i++", Add))
	assert.False(t, hasOperator("add(x, a)", Add))
	assert.True(t, hasOperator("x - a", Sub))
	assert.False(t, hasOperator("i--", Sub))
	assert.True(t, hasOperator("x * a", Mul))
	assert.False(t, hasOperator("x ** a", Mul))
	assert.True(t, hasOperator("x - a", AnyArithmetic))
	assert.False(t, hasOperator("x / a", AnyArithmetic))
}
