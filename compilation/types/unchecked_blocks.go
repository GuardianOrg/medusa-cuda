package types

// WrappingBlockSpans returns the byte ranges [start, end) of the Solidity blocks in which arithmetic wraps on
// overflow instead of reverting from Solidity 0.8 on: `unchecked { ... }` blocks and inline `assembly { ... }`
// blocks. Each range runs from the opening brace to just past the closing one. Comments and string literals are
// skipped, so braces and keywords inside them do not count.
func WrappingBlockSpans(code []byte) [][2]int {
	type openBrace struct {
		pos      int
		wrapping bool
	}
	var (
		spans     [][2]int
		stack     []openBrace
		lastIdent string
	)

	isIdentStart := func(b byte) bool {
		return b == '_' || b == '$' || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
	}
	isIdentPart := func(b byte) bool {
		return isIdentStart(b) || (b >= '0' && b <= '9')
	}

	for i := 0; i < len(code); i++ {
		b := code[i]
		switch {
		case b == '/' && i+1 < len(code) && code[i+1] == '/':
			for i < len(code) && code[i] != '\n' {
				i++
			}
		case b == '/' && i+1 < len(code) && code[i+1] == '*':
			i += 2
			for i+1 < len(code) && !(code[i] == '*' && code[i+1] == '/') {
				i++
			}
			i++
		case b == '"' || b == '\'':
			// A string literal, including the quoted part of hex"..." and unicode"...".
			for i++; i < len(code) && code[i] != b; i++ {
				if code[i] == '\\' {
					i++
				}
			}
			// `assembly "evmasm" {` and `assembly ("memory-safe") {` put strings before the brace.
			if lastIdent != "assembly" {
				lastIdent = ""
			}
		case isIdentStart(b):
			start := i
			for i+1 < len(code) && isIdentPart(code[i+1]) {
				i++
			}
			lastIdent = string(code[start : i+1])
		case b == '{':
			stack = append(stack, openBrace{pos: i, wrapping: lastIdent == "unchecked" || lastIdent == "assembly"})
			lastIdent = ""
		case b == '}':
			if n := len(stack); n > 0 {
				open := stack[n-1]
				stack = stack[:n-1]
				if open.wrapping {
					spans = append(spans, [2]int{open.pos, i + 1})
				}
			}
			lastIdent = ""
		case b == ' ' || b == '\t' || b == '\n' || b == '\r':
			// Whitespace keeps the last identifier, so `unchecked {` and `assembly {` are found.
		case b == '(' || b == ')':
			// Keep the identifier across the flags of `assembly ("memory-safe") {`, which are a string in
			// parentheses. Other parentheses never come between `unchecked` or `assembly` and their brace.
			if lastIdent != "assembly" {
				lastIdent = ""
			}
		default:
			lastIdent = ""
		}
	}
	return spans
}

// InWrappingBlock reports whether a source range lies inside an `unchecked` or inline `assembly` block of its file.
// It returns false for a range in a generated source.
func (c *Compilation) InWrappingBlock(r *SourceRange) (bool, error) {
	if r.IsGenerated() {
		return false, nil
	}
	code, err := c.sourceCode(r.SourcePath)
	if err != nil {
		return false, err
	}
	for _, span := range WrappingBlockSpans(code) {
		if r.Offset >= span[0] && r.Offset+r.Length <= span[1] {
			return true, nil
		}
	}
	return false, nil
}
