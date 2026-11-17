package platforms

// This file reads what Foundry records about its own builds: which compiler built each compilation unit, and which
// id that compiler gave each of the unit's files.
//
// A source map names a file by its compiler id. solc numbers a unit's files in path order, which is the order
// crytic-compile lists them in, and it writes the same ids into the AST, so medusa can read them from there. Solar
// numbers them in another order and leaves the ASTs empty, so a unit Foundry built with Solar takes its ids from the
// build-info that crytic-compile read it from.
//
// Ported from echidna_custom (lib/Echidna/Solidity/Foundry.hs, GuardianAudits/echidna_custom#23 and follow-ups).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// foundryCompiler is the compiler that built a Foundry compilation unit.
type foundryCompiler int

const (
	foundryCompilerSolc foundryCompiler = iota
	foundryCompilerSolar
)

// String returns the compiler's name.
func (c foundryCompiler) String() string {
	if c == foundryCompilerSolar {
		return "solar"
	}
	return "solc"
}

// compilerFromVersion tells the compiler from a build-info's solcLongVersion. Foundry records Solar under versions
// such as "0.8.38+commit.a2b44354.solar.0.3.0-dev" and solc under plain ones.
func compilerFromVersion(version string) foundryCompiler {
	if strings.Contains(strings.ToLower(version), "solar") {
		return foundryCompilerSolar
	}
	return foundryCompilerSolc
}

// foundryBuild describes one Foundry build-info file.
type foundryBuild struct {
	// BuildId is the build-info's file name without its extension. crytic-compile gives a unit's export the same
	// name when a project has several units.
	BuildId string

	// Compiler is the compiler that built the unit.
	Compiler foundryCompiler

	// SourceIds maps each file of the unit to the id its compiler gave it. Paths are as Foundry wrote them (relative
	// to the project root), except that files outside the root are made absolute by foundryPath, and they always use
	// forward slashes.
	SourceIds map[string]int
}

// isFoundryProject reports whether the directory is the root of a Foundry project.
func isFoundryProject(root string) bool {
	info, err := os.Stat(filepath.Join(root, "foundry.toml"))
	return err == nil && !info.IsDir()
}

// readFoundryBuilds returns the builds crytic-compile read for the Foundry project at the given root: every
// build-info in <out>/build-info, where <out> is the --foundry-out-directory crytic-compile was given, or else the
// active profile's out directory. Each one is a unit of crytic-compile's export. It returns none when the root is not
// a Foundry project.
func readFoundryBuilds(cryticArgs []string, root string) ([]foundryBuild, error) {
	if !isFoundryProject(root) {
		return nil, nil
	}
	out, ok := foundryOutArg(cryticArgs)
	if !ok {
		out = forgeOut(root)
	}
	if !filepath.IsAbs(out) {
		out = filepath.Join(root, out)
	}
	return readBuildInfoDir(root, filepath.Join(out, "build-info"))
}

// foundryOutArg returns the out directory that crytic-compile's --foundry-out-directory argument names, accepting
// both the "--foundry-out-directory dir" and the "--foundry-out-directory=dir" forms.
func foundryOutArg(args []string) (string, bool) {
	const flag = "--foundry-out-directory"
	for i, arg := range args {
		if arg == flag && i+1 < len(args) && args[i+1] != "" {
			return args[i+1], true
		}
		if dir, found := strings.CutPrefix(arg, flag+"="); found && dir != "" {
			return dir, true
		}
	}
	return "", false
}

// forgeOut returns the active profile's out directory as `forge config --json` resolves it, or Foundry's default
// ("out") when forge can't tell.
func forgeOut(root string) string {
	cmd := exec.Command("forge", "config", "--json")
	cmd.Dir = root
	stdout, err := cmd.Output()
	if err != nil {
		return "out"
	}
	var config struct {
		Out string `json:"out"`
	}
	if err := json.Unmarshal(stdout, &config); err != nil || config.Out == "" {
		return "out"
	}
	return config.Out
}

// readBuildInfoDir reads every readable build-info file in a directory, in file name order. Files that are not valid
// build-info files are skipped.
func readBuildInfoDir(root string, dir string) ([]foundryBuild, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("could not read Foundry build-info directory '%s': %w", dir, err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	builds := make([]foundryBuild, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		version, ids, err := parseBuildInfo(data)
		if err != nil {
			continue
		}
		sourceIds := make(map[string]int, len(ids))
		for path, id := range ids {
			sourceIds[filepath.ToSlash(foundryPath(absRoot, path))] = id
		}
		builds = append(builds, foundryBuild{
			BuildId:   strings.TrimSuffix(name, filepath.Ext(name)),
			Compiler:  compilerFromVersion(version),
			SourceIds: sourceIds,
		})
	}
	return builds, nil
}

// parseBuildInfo returns a build-info file's compiler version and its file ids, keyed by path relative to the
// project root. Newer Foundry versions write the ids as source_id_to_path, older ones only in the compiler output.
func parseBuildInfo(data []byte) (string, map[string]int, error) {
	var info struct {
		SolcLongVersion string            `json:"solcLongVersion"`
		SourceIdToPath  map[string]string `json:"source_id_to_path"`
		Output          json.RawMessage   `json:"output"`
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return "", nil, err
	}

	ids := make(map[string]int)
	if info.SourceIdToPath != nil {
		for idStr, path := range info.SourceIdToPath {
			id, err := strconv.Atoi(idStr)
			if err != nil {
				return "", nil, fmt.Errorf("source id is not a number: %s", idStr)
			}
			ids[path] = id
		}
		return info.SolcLongVersion, ids, nil
	}

	var output struct {
		Sources map[string]struct {
			Id *int `json:"id"`
		} `json:"sources"`
	}
	if len(info.Output) == 0 {
		return "", nil, errors.New("build-info has neither source_id_to_path nor output")
	}
	if err := json.Unmarshal(info.Output, &output); err != nil {
		return "", nil, err
	}
	if output.Sources == nil {
		return "", nil, errors.New("build-info output has no sources")
	}
	for path, source := range output.Sources {
		if source.Id == nil {
			return "", nil, fmt.Errorf("build-info output source '%s' has no id", path)
		}
		ids[path] = *source.Id
	}
	return info.SolcLongVersion, ids, nil
}

// foundryPath normalizes a path from a build-info. A file outside the project root is written relative to it
// ("../x.sol"), so it is made absolute. Links are not resolved and case is kept: two imports of one file that differ
// only in case are two files to the compiler.
func foundryPath(absRoot string, path string) string {
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		if part == ".." {
			return filepath.Join(absRoot, path)
		}
	}
	return filepath.Clean(path)
}

// compilerIdsFor returns the compiler ids of a unit's files, keyed by the files' positions in crytic-compile's
// sourceList, when Foundry built the unit with Solar. The unit is the build that lists exactly its files:
// crytic-compile's paths may be absolute, so each one matches the longest trailing part of it that the build lists.
//
// It returns nil ids and a nil error when the unit was built by solc (or no build-info matches and none is Solar),
// meaning the ids medusa already has are the compiler's. It returns an error explaining why a Solar unit could not be
// matched.
func compilerIdsFor(builds []foundryBuild, files map[int]string) (map[int]int, error) {
	type match struct {
		compiler foundryCompiler
		ids      map[int]int
	}

	var matches []match
	anySolar := false
	for _, build := range builds {
		if build.Compiler == foundryCompilerSolar {
			anySolar = true
		}
		ids, ok := idsInBuild(build, files)
		if !ok {
			continue
		}
		duplicate := false
		for _, m := range matches {
			if m.compiler == build.Compiler && sameIds(m.ids, ids) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			matches = append(matches, match{compiler: build.Compiler, ids: ids})
		}
	}

	switch {
	case len(matches) == 0 && anySolar:
		return nil, errors.New("no Solar build-info lists exactly this unit's files")
	case len(matches) == 0:
		return nil, nil
	case len(matches) == 1 && matches[0].compiler == foundryCompilerSolar:
		return matches[0].ids, nil
	}
	for _, m := range matches {
		if m.compiler == foundryCompilerSolar {
			return nil, errors.New("several build-infos list this unit's files with different ids")
		}
	}
	return nil, nil
}

// idsInBuild maps each of a unit's files to its id in the build, if the build lists exactly the unit's files and
// gives each of them its own id.
func idsInBuild(build foundryBuild, files map[int]string) (map[int]int, bool) {
	if len(build.SourceIds) != len(files) {
		return nil, false
	}
	ids := make(map[int]int, len(files))
	seen := make(map[int]bool, len(files))
	for position, path := range files {
		id, ok := lookupPathSuffix(build.SourceIds, path)
		if !ok || seen[id] {
			return nil, false
		}
		seen[id] = true
		ids[position] = id
	}
	return ids, true
}

// sameIds reports whether two id maps are equal.
func sameIds(a map[int]int, b map[int]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			return false
		}
	}
	return true
}

// lookupPathSuffix returns the entry for the longest trailing part of a path that the map lists. The map's keys must
// use forward slashes. Paths are compared as text, case included.
func lookupPathSuffix(m map[string]int, path string) (int, bool) {
	parts := strings.Split(filepath.ToSlash(filepath.Clean(path)), "/")
	for n := 0; n < len(parts); n++ {
		suffix := strings.Join(parts[n:], "/")
		if suffix == "" {
			continue
		}
		if id, ok := m[suffix]; ok {
			return id, true
		}
	}
	return 0, false
}
