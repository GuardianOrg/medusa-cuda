package platforms

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crytic/medusa/compilation/types"
	"github.com/crytic/medusa/logging"
	"github.com/crytic/medusa/utils"
)

// CryticCompilationConfig represents the various configuration options that can be provided by the user
// while using the `crytic-compile` platform
type CryticCompilationConfig struct {
	// Target is the object that is being compiled. It can be a single `.sol` file or a whole directory
	Target string `json:"target"`

	// SolcVersion is the version of `solc` that will be installed prior to compiling with crytic-compile. If empty,
	// no special version is installed prior to compilation.
	SolcVersion string `json:"solcVersion"`

	// ExportDirectory is the location to search for exported build artifacts. By default, we look in `./crytic-export`
	ExportDirectory string `json:"exportDirectory"`

	// Args are additional arguments that can be provided to `crytic-compile`
	Args []string `json:"args"`

	// SkipSolcInstall is a flag to skip the installation, this assumes that the solc version is already installed by solc-select
	SkipSolcInstall bool `json:"skipSolcInstall"`

	// EtherscanJsonFile is the path to the json file downloaded from etherscan to compile
	EtherscanJsonFile bool `json:"etherscanJsonFile"`
}

// Platform returns the platform type
func (c *CryticCompilationConfig) Platform() string {
	return "crytic-compile"
}

// GetTarget returns the target for compilation
func (c *CryticCompilationConfig) GetTarget() string {
	if c.EtherscanJsonFile {
		return "CUEVM_ETHERSCAN_TARGET" + c.Target
	}
	return c.Target
}

// SetTarget sets the new target for compilation
func (c *CryticCompilationConfig) SetTarget(newTarget string) {
	c.Target = newTarget
}

// NewCryticCompilationConfig returns the default configuration options while using `crytic-compile`
func NewCryticCompilationConfig(target string) *CryticCompilationConfig {
	return &CryticCompilationConfig{
		Target:          target,
		ExportDirectory: "",
		Args:            []string{},
		SolcVersion:     "",
	}
}

// validateArgs ensures that the additional arguments provided to `crytic-compile` do not contain the `--export-format`
// or the `--export-dir` arguments. This is because `--export-format` has to be `solc` for the `crytic-compile`
// integration to work and CryticCompilationConfig.BuildDirectory option is equivalent to `--export-dir`
func (c *CryticCompilationConfig) validateArgs() error {
	// If --export-format or --export-dir are specified in c.Args, throw an error
	for _, arg := range c.Args {
		if arg == "--export-format" {
			return errors.New("do not specify `--export-format` within crytic-compile arguments as the solc export format is always used")
		}
		if arg == "--export-dir" {
			return errors.New("do not specify `--export-dir` as an argument, use the BuildDirectory config variable instead")
		}
	}
	return nil
}

// getArgs returns the arguments to be provided to crytic-compile during compilation, or an error if one occurs.
func (c *CryticCompilationConfig) getArgs() ([]string, error) {
	// By default we export in solc mode.
	args := []string{c.Target, "--export-format", "solc"}

	// Add --export-dir option if ExportDirectory is specified
	if c.ExportDirectory != "" {
		args = append(args, "--export-dir", c.ExportDirectory)
	}

	// Add remaining args
	args = append(args, c.Args...)
	return args, nil
}

// Compile uses the CryticCompilationConfig provided to compile a given target, parse the artifacts, and then
// create a list of types.Compilation.
func (c *CryticCompilationConfig) Compile() ([]types.Compilation, string, error) {
	// Resolve our export directory and delete it if already exists
	exportDirectory := c.ExportDirectory
	if exportDirectory == "" {
		exportDirectory = "crytic-export"
	}
	err := utils.DeleteDirectory(exportDirectory)
	if err != nil {
		return nil, "", fmt.Errorf("could not delete crytic-compile's export directory prior to compilation, error: %v", err)
	}

	// Validate args to make sure --export-format and --export-dir are not specified
	err = c.validateArgs()
	if err != nil {
		return nil, "", err
	}

	// Fetch the arguments to invoke crytic-compile with
	args, err := c.getArgs()
	if err != nil {
		return nil, "", err
	}

	if c.EtherscanJsonFile {
		args = append(args, "--compile-force-framework", "etherscan", "--etherscan-json-file", c.Target)
		fmt.Printf("CuEVM Debug: args: %s\n", args)
	}
	fmt.Printf("CuEVM Debug: args: %s\n", args)
	// Get main command and set working directory
	cmd := exec.Command("crytic-compile", args...)
	logging.GlobalLogger.Info("Running command:\n", cmd.String())

	// Install a specific `solc` version if requested in the config
	if c.SolcVersion != "" {
		if !c.SkipSolcInstall {
			out, err := exec.Command("solc-select", "install", c.SolcVersion).CombinedOutput()
			if err != nil {
				return nil, "", fmt.Errorf("error while executing `solc-select install`:\nOUTPUT:\n%s\nERROR: %s\n", string(out), err.Error())
			}
		}

		out, err := exec.Command("solc-select", "use", c.SolcVersion).CombinedOutput()
		if err != nil {
			return nil, "", fmt.Errorf("error while executing `solc-select use`:\nOUTPUT:\n%s\nERROR: %s\n", string(out), err.Error())
		}
	}

	// Run crytic-compile to compile and export our compilation artifacts.
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, "", fmt.Errorf("error while executing crytic-compile:\nOUTPUT:\n%s\nERROR: %s\n", string(out), err.Error())
	}

	// Read what Foundry recorded about the builds crytic-compile exported. A unit Foundry built with a Solar that
	// does not write ASTs takes its file ids from there. Without a Foundry project there are no builds.
	var foundryBuilds []foundryBuild
	if !c.EtherscanJsonFile {
		foundryBuilds, err = readFoundryBuilds(c.Args, c.Target)
		if err != nil {
			logging.GlobalLogger.Warn("could not read Foundry build-info files, source maps of Solar builds may ",
				"resolve to the wrong files: ", err)
		}
	}

	// Parse each exported compilation unit.
	compilationList, err := readCryticExports(exportDirectory, foundryBuilds)
	if err != nil {
		return nil, "", err
	}
	return compilationList, string(out), nil
}

// solcSourceUnit is a source unit of crytic-compile's solc export.
type solcSourceUnit struct {
	AST any `json:"AST"`
}

// solcExportContract is a contract of crytic-compile's solc export.
type solcExportContract struct {
	SrcMap        string `json:"srcmap"`
	SrcMapRuntime string `json:"srcmap-runtime"`
	Abi           any    `json:"abi"`
	Bin           string `json:"bin"`
	BinRuntime    string `json:"bin-runtime"`
}

// solcExportData is crytic-compile's solc export of one compilation unit.
type solcExportData struct {
	Sources    map[string]solcSourceUnit     `json:"sources"`
	SourceList []string                      `json:"sourceList"`
	Contracts  map[string]solcExportContract `json:"contracts"`
}

// ReadCryticExports parses every crytic-compile solc export in a directory, one per compilation unit, as Compile does
// after running crytic-compile. If foundryRoot is the root of a Foundry project, the file ids of units Foundry built
// with a Solar that does not write ASTs are read from its build-info files, found as crytic-compile would find them
// with the given crytic-compile arguments.
func ReadCryticExports(exportDirectory string, foundryRoot string, cryticArgs []string) ([]types.Compilation, error) {
	var foundryBuilds []foundryBuild
	if foundryRoot != "" {
		var err error
		foundryBuilds, err = readFoundryBuilds(cryticArgs, foundryRoot)
		if err != nil {
			return nil, err
		}
	}
	return readCryticExports(exportDirectory, foundryBuilds)
}

// readCryticExports parses every crytic-compile solc export in a directory, one per compilation unit, in file name
// order. foundryBuilds are the Foundry build-infos crytic-compile read, if any.
func readCryticExports(exportDirectory string, foundryBuilds []foundryBuild) ([]types.Compilation, error) {
	// Find compilation artifacts in the export directory
	matches, err := filepath.Glob(filepath.Join(exportDirectory, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)

	compilationList := make([]types.Compilation, 0, len(matches))
	for _, exportPath := range matches {
		compilation, err := readCryticExport(exportPath, foundryBuilds)
		if err != nil {
			return nil, err
		}
		compilationList = append(compilationList, *compilation)
	}
	return compilationList, nil
}

// readCryticExport parses one crytic-compile solc export into a types.Compilation.
func readCryticExport(exportPath string, foundryBuilds []foundryBuild) (*types.Compilation, error) {
	// Read the compiled JSON file data
	b, err := os.ReadFile(exportPath)
	if err != nil {
		return nil, fmt.Errorf("could not parse crytic-compile's exported solc data at path '%s', error: %v", exportPath, err)
	}

	// Parse the JSON
	var solcExport solcExportData
	err = json.Unmarshal(b, &solcExport)
	if err != nil {
		return nil, fmt.Errorf("could not parse crytic-compile's exported solc data, error: %v", err)
	}

	// Create a compilation object that will store the contracts and source information.
	compilation := types.NewCompilation()

	// Create a map of contract names to their kinds
	contractKinds := make(map[string]types.ContractKind)

	// Find the id the compiler gave each source, which its source maps use to name the file.
	sourcePaths := make([]string, 0, len(solcExport.Sources))
	astIds := make(map[string]int, len(solcExport.Sources))
	for sourcePath, source := range solcExport.Sources {
		sourcePaths = append(sourcePaths, sourcePath)
		astIds[sourcePath] = astSourceUnitId(source.AST)
	}
	sourceIds, idErr := sourceUnitIds(filepath.Base(exportPath), sourcePaths, solcExport.SourceList, astIds,
		foundryBuilds)
	if idErr != nil {
		logging.GlobalLogger.Warn(exportPath, ": ", idErr, ", so its coverage may be reported on the wrong files")
	}

	// Loop through all sources and parse them into our types.
	for sourcePath, source := range solcExport.Sources {
		// Convert the AST into our version of the AST (types.AST)
		var ast types.AST
		b, err = json.Marshal(source.AST)
		if err != nil {
			return nil, fmt.Errorf("could not encode AST from sources: %v", err)
		}
		err = json.Unmarshal(b, &ast)
		if err != nil {
			return nil, fmt.Errorf("could not parse AST from sources: %v", err)
		}

		// From the AST, extract the contract kinds where the contract definition could be for a contract, library,
		// or interface
		for _, node := range ast.Nodes {
			if node.GetNodeType() == "ContractDefinition" {
				contractDefinition := node.(types.ContractDefinition)
				contractKinds[contractDefinition.CanonicalName] = contractDefinition.Kind
			}
		}

		// Retrieve the source unit ID
		sourceUnitId, ok := sourceIds[sourcePath]
		if !ok {
			sourceUnitId = -1
		}
		compilation.SourcePathToArtifact[sourcePath] = types.SourceArtifact{
			// TODO: Our types.AST is not the same as the original AST but we could parse it and avoid using "any"
			Ast:          source.AST,
			Contracts:    make(map[string]types.CompiledContract),
			SourceUnitId: sourceUnitId,
		}
		if sourceUnitId >= 0 {
			compilation.SourceIdToPath[sourceUnitId] = sourcePath
		}
	}

	// Loop through all contracts and parse them into our types.
	for sourceAndContractPath, contract := range solcExport.Contracts {
		// Split our source and contract path, as it takes the form sourcePath:contractName
		splitIndex := strings.LastIndex(sourceAndContractPath, ":")
		if splitIndex == -1 {
			return nil, fmt.Errorf("expected contract path to be of form \"<source path>:<contract_name>\"")
		}
		sourcePath := sourceAndContractPath[:splitIndex]
		contractName := sourceAndContractPath[splitIndex+1:]

		// Ensure a source exists for this, or create one if our path somehow differed from any
		// path not existing in the "sources" key at the root of the export.
		if _, ok := compilation.SourcePathToArtifact[sourcePath]; !ok {
			parentSource := types.SourceArtifact{
				Ast:       nil,
				Contracts: make(map[string]types.CompiledContract),
			}
			compilation.SourcePathToArtifact[sourcePath] = parentSource
		}

		// Parse the ABI
		contractAbi, err := types.ParseABIFromInterface(contract.Abi)
		if err != nil {
			return nil, fmt.Errorf("unable to parse ABI for contract '%s'\n", contractName)
		}

		// Decode our init and runtime bytecode
		initBytecode, err := hex.DecodeString(strings.TrimPrefix(contract.Bin, "0x"))
		if err != nil {
			return nil, fmt.Errorf("unable to parse init bytecode for contract '%s'\n", contractName)
		}
		runtimeBytecode, err := hex.DecodeString(strings.TrimPrefix(contract.BinRuntime, "0x"))
		if err != nil {
			return nil, fmt.Errorf("unable to parse runtime bytecode for contract '%s'\n", contractName)
		}

		// Add contract details
		compilation.SourcePathToArtifact[sourcePath].Contracts[contractName] = types.CompiledContract{
			Abi:             *contractAbi,
			InitBytecode:    initBytecode,
			RuntimeBytecode: runtimeBytecode,
			SrcMapsInit:     contract.SrcMap,
			SrcMapsRuntime:  contract.SrcMapRuntime,
			Kind:            contractKinds[contractName],
		}
	}

	return compilation, nil
}

// astSourceUnitId returns the source unit id in the "src" field of a source unit's AST, or -1 when the AST has none.
// solc always writes it. Solar writes it from paradigmxyz/solar#2344 on, and leaves the AST empty before that.
func astSourceUnitId(ast any) int {
	node, ok := ast.(map[string]any)
	if !ok {
		return -1
	}
	src, ok := node["src"].(string)
	if !ok {
		return -1
	}
	return types.GetSrcMapSourceUnitID(src)
}

// sourceUnitIds returns the id the compiler gave each source of one crytic-compile export, the id its source maps
// name the file by. In order of preference, the id comes from:
//
//   - the source unit's AST, which solc and Solar from paradigmxyz/solar#2344 on write;
//   - the Foundry build-info of the unit, when Foundry built it with a Solar that leaves ASTs empty, since Solar does
//     not number files in the order crytic-compile lists them;
//   - the source's position in the export's sourceList, which is solc's id order.
//
// A Solar build-info that disagrees with the AST wins, as it holds the ids Solar used for its source maps. An error
// explains why a Solar unit without ASTs could not be matched to its build-info; the ids returned then are the
// sourceList positions.
func sourceUnitIds(exportName string, sourcePaths []string, sourceList []string, astIds map[string]int,
	builds []foundryBuild) (map[string]int, error) {
	// Number the sources by their position in the sourceList. An export without one lists them in path order.
	positions := sourceList
	if len(positions) == 0 {
		positions = append([]string(nil), sourcePaths...)
		sort.Strings(positions)
	}
	files := make(map[int]string, len(positions))
	positionOf := make(map[string]int, len(positions))
	for position, path := range positions {
		files[position] = path
		positionOf[path] = position
	}

	// With several units, crytic-compile names each export after its build-info, so that build-info is used first.
	candidates := builds
	exportId := strings.TrimSuffix(exportName, filepath.Ext(exportName))
	for _, build := range builds {
		if build.BuildId == exportId {
			candidates = []foundryBuild{build}
			break
		}
	}

	// Only consult the build-infos when the export spans the sources it lists.
	var solarIds map[int]int
	var solarErr error
	if len(files) == len(sourcePaths) {
		solarIds, solarErr = compilerIdsFor(candidates, files)
	}

	allAstIds := true
	for _, path := range sourcePaths {
		if astIds[path] < 0 {
			allAstIds = false
			break
		}
	}

	ids := make(map[string]int, len(sourcePaths))
	for _, path := range sourcePaths {
		position, listed := positionOf[path]
		switch {
		case solarIds != nil && listed:
			ids[path] = solarIds[position]
		case astIds[path] >= 0:
			ids[path] = astIds[path]
		case listed:
			ids[path] = position
		default:
			ids[path] = -1
		}
	}

	// A unit with ASTs has its ids already, so a build-info that can't be matched does not matter.
	if solarErr != nil && !allAstIds {
		return ids, solarErr
	}
	return ids, nil
}
