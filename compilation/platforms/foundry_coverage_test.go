package platforms

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/crytic/medusa/compilation/types"
	"github.com/crytic/medusa/fuzzing/coverage"
	"github.com/crytic/medusa/logging"
	"github.com/crytic/medusa/utils/testutils"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// activeLines runs medusa's source coverage analysis on a compilation and returns its executable lines as
// "<file base name>:<line number>".
func activeLines(t *testing.T, compilation *types.Compilation) []string {
	t.Helper()
	require.NoError(t, compilation.CacheSourceCode())
	analysis, err := coverage.AnalyzeSourceCoverage([]types.Compilation{*compilation}, coverage.NewCoverageMaps(),
		logging.NewLogger(zerolog.Disabled))
	require.NoError(t, err)

	var lines []string
	for _, file := range analysis.SortedFiles() {
		for i, line := range file.Lines {
			if line.IsActive {
				lines = append(lines, fmt.Sprintf("%s:%d", filepath.Base(file.Path), i+1))
			}
		}
	}
	return lines
}

// TestFoundryCoverageLines checks that medusa's coverage analysis marks code lines executable in the files that hold
// them, for each compiler. Each file's code is on one line (Main.sol:4, Alpha.sol:4, Zlib.sol:3), and the lines
// above it hold only the license, the pragma and imports.
func TestFoundryCoverageLines(t *testing.T) {
	codeLines := []string{"Alpha.sol:4", "Main.sol:4", "Zlib.sol:3"}
	for _, project := range []string{"solc", "solar-noast", "solar-ast"} {
		t.Run(project, func(t *testing.T) {
			builds, err := readFoundryBuilds(nil, filepath.Join("testdata", "foundry", project))
			require.NoError(t, err)
			assert.ElementsMatch(t, codeLines, activeLines(t, readFixture(t, project, builds)))
		})
	}

	// Without its build-info, a Solar build without ASTs marks lines in the wrong files.
	wrong := activeLines(t, readFixture(t, "solar-noast", nil))
	assert.NotSubset(t, wrong, codeLines)
	t.Logf("solar-noast without its build-info marks %v", wrong)
}

// TestCryticCompileFoundrySolar compiles the three-file project with crytic-compile, Foundry and the installed Solar,
// and checks that coverage lines land in the right files, whether this Solar writes ASTs or not.
func TestCryticCompileFoundrySolar(t *testing.T) {
	for _, tool := range []string{"crytic-compile", "forge", "solar"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}

	projectPath := testutils.CopyToTestDirectory(t, "testdata/foundry/solar-ast")
	require.NoError(t, os.RemoveAll(filepath.Join(projectPath, "out")))
	require.NoError(t, os.RemoveAll(filepath.Join(projectPath, "export")))

	testutils.ExecuteInDirectory(t, projectPath, func() {
		compilations, _, err := NewCryticCompilationConfig(".").Compile()
		require.NoError(t, err)
		require.Len(t, compilations, 1)

		builds, err := readFoundryBuilds(nil, ".")
		require.NoError(t, err)
		require.Len(t, builds, 1)
		require.Equal(t, foundryCompilerSolar, builds[0].Compiler, "foundry.toml must build with Solar")

		hasAst := false
		for _, source := range compilations[0].SourcePathToArtifact {
			hasAst = hasAst || astSourceUnitId(source.Ast) >= 0
		}
		t.Logf("Solar wrote ASTs: %v", hasAst)

		assert.ElementsMatch(t, []string{"Alpha.sol:4", "Main.sol:4", "Zlib.sol:3"}, activeLines(t, &compilations[0]))
		assert.Equal(t, map[string]int{"Zlib.sol": 0, "Alpha.sol": 1, "Main.sol": 2}, sourceIdsByFile(&compilations[0]))
	})
}
