// Package bugfilter decides which of the bugs CuEVM reports are false positives, from the source code the bug's
// program counter maps to.
package bugfilter

import (
	"fmt"
	"strings"

	"github.com/crytic/medusa/compilation/types"
)

// ArithmeticOp is the operation of an arithmetic bug CuEVM reports.
type ArithmeticOp int

const (
	// AnyArithmetic is an integer bug of no particular operation.
	AnyArithmetic ArithmeticOp = iota
	// Add is an addition that overflowed.
	Add
	// Sub is a subtraction that underflowed.
	Sub
	// Mul is a multiplication that overflowed.
	Mul
)

// String returns the operation's name.
func (op ArithmeticOp) String() string {
	switch op {
	case Add:
		return "ADD"
	case Sub:
		return "SUB"
	case Mul:
		return "MUL"
	}
	return "INTEGER_BUG"
}

// ArithmeticVerdict is the outcome of checking an arithmetic bug.
type ArithmeticVerdict struct {
	// FalsePositive is true when the bug is not a real overflow in the contract's code.
	FalsePositive bool

	// Reason explains the verdict.
	Reason string

	// Source is the source code the bug's program counter maps to, or nil when it maps to none.
	Source *types.SourceRange
}

// CheckArithmeticBug decides whether an arithmetic bug CuEVM reported at a program counter of a contract's runtime
// bytecode is a false positive. The program counter is resolved with the source map of the compilation that built
// the bytecode, so this works for solc and Solar builds alike.
//
// A bug is a false positive when:
//   - its program counter is 0 or maps to no source;
//   - its source range spans 3 lines or more, as a function or contract definition does;
//   - it maps to a source the compiler generated, as solc's checked arithmetic helpers in Yul;
//   - the source code lacks the operator of the bug (or has `++`, `--`, `**` instead);
//   - the compiler is solc 0.8 or later, or Solar, and the code is outside `unchecked` and `assembly` blocks, so
//     the arithmetic is checked and reverts on overflow. Solar maps checked arithmetic to the expression itself, not
//     to a generated helper, so this is how its checked arithmetic is told apart. CuEVM still reports such an
//     overflow when a caller catches the revert.
func CheckArithmeticBug(compilation *types.Compilation, contract *types.CompiledContract, pc uint64,
	op ArithmeticOp) ArithmeticVerdict {
	if pc == 0 {
		return ArithmeticVerdict{FalsePositive: true, Reason: "contract-level program counter"}
	}

	source, err := compilation.SourceRangeAtPC(contract, pc, true)
	if err != nil {
		return ArithmeticVerdict{FalsePositive: true, Reason: fmt.Sprintf("no source: %v", err)}
	}
	verdict := ArithmeticVerdict{FalsePositive: true, Source: source}

	if span := source.EndLine - source.StartLine; span >= 3 {
		verdict.Reason = fmt.Sprintf("source range spans %d lines", span+1)
		return verdict
	}
	if source.IsGenerated() {
		verdict.Reason = "compiler-generated source"
		return verdict
	}

	fragment := strings.TrimSpace(source.Fragment)
	if !hasOperator(fragment, op) {
		verdict.Reason = fmt.Sprintf("source has no %s operator: %q", op, fragment)
		return verdict
	}

	if compiler, ok := types.CompilerFromMetadata(contract.RuntimeBytecode); ok && compiler.ChecksArithmetic() {
		wrapping, err := compilation.InWrappingBlock(source)
		if err != nil {
			verdict.Reason = fmt.Sprintf("could not read source: %v", err)
			return verdict
		}
		if !wrapping {
			verdict.Reason = fmt.Sprintf("checked arithmetic (%s %d.%d.%d) outside unchecked blocks: %q",
				compiler.Name, compiler.Major, compiler.Minor, compiler.Patch, fragment)
			return verdict
		}
	}

	verdict.FalsePositive = false
	verdict.Reason = fmt.Sprintf("%s in %q", op, fragment)
	return verdict
}

// hasOperator reports whether source code holds the operator of an arithmetic bug, and not only its increment,
// decrement or exponent form.
func hasOperator(fragment string, op ArithmeticOp) bool {
	switch op {
	case Add:
		return strings.Contains(fragment, "+") && !strings.Contains(fragment, "++")
	case Sub:
		return strings.Contains(fragment, "-") && !strings.Contains(fragment, "--")
	case Mul:
		return strings.Contains(fragment, "*") && !strings.Contains(fragment, "**")
	}
	return strings.ContainsAny(fragment, "+-*")
}
