package platforms

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crytic/medusa/compilation/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The testdata/foundry projects are one three-file project (Main imports Alpha, Alpha imports the Zlib library)
// built by Foundry with different compilers:
//   - solc: solc numbers the files in path order (Main 0, Alpha 1, Zlib 2) and writes ASTs.
//   - solar-noast: a Solar from before paradigmxyz/solar#2344 numbers them in reverse (Zlib 0, Alpha 1, Main 2) and
//     leaves the ASTs empty, so the ids must come from the build-info. Its out-old holds a solc build of the files.
//   - solar-ast: Solar main, which numbers them in reverse and writes solc-compatible ASTs carrying those ids.

// buildSummary is a build's id and compiler, for comparing the builds read from a project.
type buildSummary struct {
	id       string
	compiler foundryCompiler
}

func summarize(builds []foundryBuild) []buildSummary {
	summaries := make([]buildSummary, 0, len(builds))
	for _, build := range builds {
		summaries = append(summaries, buildSummary{build.BuildId, build.Compiler})
	}
	return summaries
}

// sourceSnippet is a source range a source map element names: the file's base name and the text of the range.
type sourceSnippet struct {
	file string
	text string
}

// runtimeSnippets returns the source ranges the runtime source map of a contract names, resolving each file id with
// the compilation's SourceIdToPath, as coverage reporting does.
func runtimeSnippets(t *testing.T, compilation *types.Compilation, contractName string) []sourceSnippet {
	t.Helper()
	require.NoError(t, compilation.CacheSourceCode())

	var contract *types.CompiledContract
	for _, source := range compilation.SourcePathToArtifact {
		if c, ok := source.Contracts[contractName]; ok {
			contract = &c
		}
	}
	require.NotNil(t, contract, "contract %s not found", contractName)

	sourceMap, err := types.ParseSourceMap(contract.SrcMapsRuntime)
	require.NoError(t, err)

	var snippets []sourceSnippet
	for _, element := range sourceMap {
		path, ok := compilation.SourceIdToPath[element.SourceUnitID]
		if !ok || element.Offset < 0 || element.Length < 0 {
			continue
		}
		code := compilation.SourceCode[path]
		if element.Offset+element.Length > len(code) {
			// A range past the end of the file it resolves to means the id named another file.
			snippets = append(snippets, sourceSnippet{filepath.Base(path), "<out of range>"})
			continue
		}
		snippets = append(snippets, sourceSnippet{filepath.Base(path), string(code[element.Offset : element.Offset+element.Length])})
	}
	return snippets
}

// sourceIdsByFile returns the compilation's file ids keyed by the files' base names.
func sourceIdsByFile(compilation *types.Compilation) map[string]int {
	ids := make(map[string]int)
	for id, path := range compilation.SourceIdToPath {
		ids[filepath.Base(path)] = id
	}
	return ids
}

// readFixture reads the single crytic-compile export of a testdata/foundry project.
func readFixture(t *testing.T, project string, builds []foundryBuild) *types.Compilation {
	t.Helper()
	compilations, err := readCryticExports(filepath.Join("testdata", "foundry", project, "export"), builds)
	require.NoError(t, err)
	require.Len(t, compilations, 1)
	return &compilations[0]
}

// assertMapsToOwnFiles checks that the code of each of the project's files resolves to that file.
func assertMapsToOwnFiles(t *testing.T, compilation *types.Compilation) {
	t.Helper()
	snippets := runtimeSnippets(t, compilation, "Main")
	assert.Contains(t, snippets, sourceSnippet{"Zlib.sol", "x + 1"})
	assert.Contains(t, snippets, sourceSnippet{"Main.sol", "x < 5"})
	assert.Contains(t, snippets, sourceSnippet{"Alpha.sol", "Zlib.inc(x)"})
	assert.NotContains(t, snippets, sourceSnippet{"Main.sol", "<out of range>"})
	assert.NotContains(t, snippets, sourceSnippet{"Alpha.sol", "<out of range>"})
	assert.NotContains(t, snippets, sourceSnippet{"Zlib.sol", "<out of range>"})
}

// TestFoundrySolarBuildWithoutAst checks that a unit built by a Solar that leaves ASTs empty takes its file ids from
// Foundry's build-info, and that without it Zlib's code resolves to another file.
func TestFoundrySolarBuildWithoutAst(t *testing.T) {
	builds, err := readFoundryBuilds(nil, filepath.Join("testdata", "foundry", "solar-noast"))
	require.NoError(t, err)
	assert.Equal(t, []buildSummary{{"8732b5cdf368240e", foundryCompilerSolar}}, summarize(builds))

	// Without the build-info the files keep their sourceList positions, which are solc's order, not Solar's.
	byPosition := readFixture(t, "solar-noast", nil)
	assert.Equal(t, map[string]int{"Main.sol": 0, "Alpha.sol": 1, "Zlib.sol": 2}, sourceIdsByFile(byPosition))
	assert.NotContains(t, runtimeSnippets(t, byPosition, "Main"), sourceSnippet{"Zlib.sol", "x + 1"})

	withBuilds := readFixture(t, "solar-noast", builds)
	assert.Equal(t, map[string]int{"Zlib.sol": 0, "Alpha.sol": 1, "Main.sol": 2}, sourceIdsByFile(withBuilds))
	assertMapsToOwnFiles(t, withBuilds)
}

// TestFoundrySolarBuildWithAst checks that a unit built by Solar main takes its file ids from its ASTs, which agree
// with its build-info, and that the ASTs give medusa the contract kinds.
func TestFoundrySolarBuildWithAst(t *testing.T) {
	builds, err := readFoundryBuilds(nil, filepath.Join("testdata", "foundry", "solar-ast"))
	require.NoError(t, err)
	require.Len(t, builds, 1)
	assert.Equal(t, foundryCompilerSolar, builds[0].Compiler)

	for _, withBuilds := range [][]foundryBuild{nil, builds} {
		compilation := readFixture(t, "solar-ast", withBuilds)
		assert.Equal(t, map[string]int{"Zlib.sol": 0, "Alpha.sol": 1, "Main.sol": 2}, sourceIdsByFile(compilation))
		assertMapsToOwnFiles(t, compilation)

		kinds := make(map[string]types.ContractKind)
		for _, source := range compilation.SourcePathToArtifact {
			for name, contract := range source.Contracts {
				kinds[name] = contract.Kind
			}
		}
		assert.Equal(t, map[string]types.ContractKind{
			"Main":  types.ContractKindContract,
			"Alpha": types.ContractKindContract,
			"Zlib":  types.ContractKindLibrary,
		}, kinds)
	}
}

// TestFoundrySolcBuild checks that a unit built by solc keeps the ids in its ASTs, with or without its build-info.
func TestFoundrySolcBuild(t *testing.T) {
	builds, err := readFoundryBuilds(nil, filepath.Join("testdata", "foundry", "solc"))
	require.NoError(t, err)
	assert.Equal(t, []buildSummary{{"e291cf7debf86b84", foundryCompilerSolc}}, summarize(builds))

	byAst := readFixture(t, "solc", nil)
	withBuilds := readFixture(t, "solc", builds)
	assert.Equal(t, byAst.SourceIdToPath, withBuilds.SourceIdToPath)
	assert.Equal(t, map[string]int{"Main.sol": 0, "Alpha.sol": 1, "Zlib.sol": 2}, sourceIdsByFile(withBuilds))
	assertMapsToOwnFiles(t, withBuilds)
}

// TestReadFoundryBuildsOutDirectory checks that the build-infos are read from the out directory crytic-compile was
// given, and that a directory without foundry.toml has none.
func TestReadFoundryBuildsOutDirectory(t *testing.T) {
	root := filepath.Join("testdata", "foundry", "solar-noast")
	outOld := []buildSummary{{"e291cf7debf86b84", foundryCompilerSolc}}
	compilers := func(args ...string) []buildSummary {
		builds, err := readFoundryBuilds(args, root)
		require.NoError(t, err)
		return summarize(builds)
	}
	assert.Equal(t, outOld, compilers("--foundry-out-directory=out-old"))
	assert.Equal(t, outOld, compilers("--foundry-ignore-compile", "--foundry-out-directory", "out-old"))
	assert.Equal(t, []buildSummary{{"8732b5cdf368240e", foundryCompilerSolar}}, compilers("--foundry-compile-all"))

	absOut, err := filepath.Abs(filepath.Join(root, "out-old"))
	require.NoError(t, err)
	assert.Equal(t, outOld, compilers("--foundry-out-directory", absOut))

	builds, err := readFoundryBuilds(nil, filepath.Join("testdata", "foundry"))
	require.NoError(t, err)
	assert.Empty(t, builds)

	_, ok := foundryOutArg([]string{"--foundry-out-directory"})
	assert.False(t, ok)
}

// TestCompilerFromVersion checks that Solar is told from solc by its long version.
func TestCompilerFromVersion(t *testing.T) {
	assert.Equal(t, foundryCompilerSolar, compilerFromVersion("0.8.38+commit.a2b44354.solar.0.3.0-dev"))
	assert.Equal(t, foundryCompilerSolc, compilerFromVersion("0.8.35+commit.47b9dedd"))
	assert.Equal(t, foundryCompilerSolc, compilerFromVersion("0.8.35"))
	assert.Equal(t, foundryCompilerSolc, compilerFromVersion(""))
}

// TestCompilerIdsFor checks how a unit is matched to the builds that list its files.
func TestCompilerIdsFor(t *testing.T) {
	unit := map[int]string{0: "/p/src/Main.sol", 1: "/p/src/Zlib.sol"}
	build := func(id string, compiler foundryCompiler, ids map[string]int) foundryBuild {
		return foundryBuild{BuildId: id, Compiler: compiler, SourceIds: ids}
	}
	solar := build("a", foundryCompilerSolar, map[string]int{"/p/src/Main.sol": 1, "/p/src/Zlib.sol": 0})
	solc := build("b", foundryCompilerSolc, map[string]int{"/p/src/Main.sol": 0, "/p/src/Zlib.sol": 1})
	otherSolar := build("c", foundryCompilerSolar, map[string]int{"/p/src/Other.sol": 0})
	noMatch := errors.New("no Solar build-info lists exactly this unit's files")

	check := func(builds []foundryBuild, files map[int]string, wantIds map[int]int, wantErr error) {
		t.Helper()
		ids, err := compilerIdsFor(builds, files)
		assert.Equal(t, wantIds, ids)
		assert.Equal(t, wantErr, err)
	}
	check([]foundryBuild{solar}, unit, map[int]int{0: 1, 1: 0}, nil)
	check([]foundryBuild{solc}, unit, nil, nil)
	check(nil, unit, nil, nil)
	// A build listing more files than the unit is another unit.
	check([]foundryBuild{build("d", foundryCompilerSolar,
		map[string]int{"/p/src/Main.sol": 2, "/p/src/Zlib.sol": 0, "/p/src/X.sol": 1})}, unit, nil, noMatch)
	check([]foundryBuild{solc, otherSolar}, unit, nil, nil)
	check([]foundryBuild{otherSolar}, unit, nil, noMatch)
	check([]foundryBuild{solar, solc}, unit, nil,
		errors.New("several build-infos list this unit's files with different ids"))
	solarCopy := solar
	solarCopy.BuildId = "e"
	check([]foundryBuild{solar, solarCopy}, unit, map[int]int{0: 1, 1: 0}, nil)

	// Imports that differ only in case are two files to the compiler, and a path matches the longest trailing part
	// of it the build lists.
	caseUnit := map[int]string{0: "/r/src/pre/A.sol", 1: "/r/src/Pre/A.sol", 2: "/r/lib/x/src/A.sol", 3: "/r/src/A.sol"}
	caseBuild := build("f", foundryCompilerSolar,
		map[string]int{"src/Pre/A.sol": 0, "src/pre/A.sol": 1, "src/A.sol": 2, "lib/x/src/A.sol": 3})
	check([]foundryBuild{caseBuild}, caseUnit, map[int]int{0: 1, 1: 0, 2: 3, 3: 2}, nil)
}

// TestFoundryPath checks how build-info paths are normalized.
func TestFoundryPath(t *testing.T) {
	assert.Equal(t, filepath.FromSlash("src/A.sol"), foundryPath("/r/p", "src/A.sol"))
	assert.Equal(t, filepath.FromSlash("src/A.sol"), foundryPath("/r/p", "./src/A.sol"))
	assert.Equal(t, filepath.FromSlash("/r/shared/src/A.sol"), foundryPath("/r/p", "../shared/src/A.sol"))
	assert.Equal(t, filepath.FromSlash("/abs/A.sol"), foundryPath("/r/p", "/abs/A.sol"))
}

// TestParseBuildInfoFromOutput checks that a build-info without source_id_to_path takes its ids from the compiler
// output, as older Foundry versions write it.
func TestParseBuildInfoFromOutput(t *testing.T) {
	version, ids, err := parseBuildInfo([]byte(`{"solcLongVersion": "0.8.38+commit.a2b44354.solar.0.3.0-dev",
		"output": {"sources": {"src/Main.sol": {"id": 2}, "src/z/Zlib.sol": {"id": 0}}}}`))
	require.NoError(t, err)
	assert.Equal(t, "0.8.38+commit.a2b44354.solar.0.3.0-dev", version)
	assert.Equal(t, map[string]int{"src/Main.sol": 2, "src/z/Zlib.sol": 0}, ids)

	_, _, err = parseBuildInfo([]byte(`{"source_id_to_path": {"x": "src/Main.sol"}}`))
	assert.Error(t, err)
	_, _, err = parseBuildInfo([]byte(`{"solcLongVersion": "0.8.35"}`))
	assert.Error(t, err)
}

// TestSourceUnitIdsWithoutSourceList checks that an export without a sourceList numbers its sources in path order.
func TestSourceUnitIdsWithoutSourceList(t *testing.T) {
	paths := []string{"src/z/Zlib.sol", "src/Main.sol", "src/a/Alpha.sol"}
	noAst := map[string]int{"src/z/Zlib.sol": -1, "src/Main.sol": -1, "src/a/Alpha.sol": -1}
	ids, err := sourceUnitIds("combined_solc.json", paths, nil, noAst, nil)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"src/Main.sol": 0, "src/a/Alpha.sol": 1, "src/z/Zlib.sol": 2}, ids)

	// With several units, the export named after a build-info uses that build-info.
	builds := []foundryBuild{
		{BuildId: "x", Compiler: foundryCompilerSolar, SourceIds: map[string]int{"src/Main.sol": 0, "src/a/Alpha.sol": 1, "src/z/Zlib.sol": 2}},
		{BuildId: "y", Compiler: foundryCompilerSolar, SourceIds: map[string]int{"src/Main.sol": 2, "src/a/Alpha.sol": 1, "src/z/Zlib.sol": 0}},
	}
	ids, err = sourceUnitIds("y.json", paths, nil, noAst, builds)
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"src/Main.sol": 2, "src/a/Alpha.sol": 1, "src/z/Zlib.sol": 0}, ids)

	// Two Solar builds of the same files with different ids can't be told apart, so the positions are kept.
	ids, err = sourceUnitIds("combined_solc.json", paths, nil, noAst, builds)
	assert.Error(t, err)
	assert.Equal(t, map[string]int{"src/Main.sol": 0, "src/a/Alpha.sol": 1, "src/z/Zlib.sol": 2}, ids)
}

// TestFoundryFixtureSourcesExist guards the fixtures: every source an export lists must exist on disk.
func TestFoundryFixtureSourcesExist(t *testing.T) {
	for _, project := range []string{"solc", "solar-noast", "solar-ast"} {
		compilation := readFixture(t, project, nil)
		for path := range compilation.SourcePathToArtifact {
			_, err := os.Stat(path)
			assert.NoError(t, err, "%s: %s", project, path)
			assert.True(t, strings.HasPrefix(filepath.ToSlash(path), "testdata/foundry/"+project+"/"), path)
		}
	}
}
