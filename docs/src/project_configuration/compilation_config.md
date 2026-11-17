# Compilation Configuration

The compilation configuration defines the parameters to use while compiling a target file or project.

### `platform`

- **Type**: String
- **Description**: Refers to the type of platform to be used to compile the underlying target. Currently,
  `crytic-compile` or `solc` can be used as the compilation platform.
- **Default**: `crytic-compile`

### `platformConfig`

- **Type**: Struct
- **Description**: This struct is a platform-dependent structure which offers parameters for compiling the underlying project.
  See below for the structure of `platformConfig` for each compilation platform.
- **Default**: The `platformConfig` for `crytic-compile` is the default value for this struct.

### `platformConfig` for `crytic-compile`

#### `target`

- **Type**: String
- **Description**: Refers to the target that is being compiled.
  > 🚩 Note that if you are using a compilation platform, such as Foundry or Hardhat, the default value for `target`, `.`,
  > should **not** be changed. The `.` is equivalent to telling `crytic-compile` that the entire project needs to compiled,
  > including any dependencies and remappings. In fact, unless you want to compile a single file, that has no third-party
  > imports from, for example, OpenZeppelin, the default value should not be changed.
- **Default**: `.`

#### `solcVersion`

- **Type**: String
- **Description**: Describes the version of `solc` that will be installed and then used for compilation. Note that if you
  are using a compilation platform, such as Foundry or Hardhat, this option does not need to be set.
- **Default**: ""

#### `exportDirectory`

- **Type**: String
- **Description**: Describes the directory where all compilation artifacts should be stored after compilation. Leaving it
  empty will lead to the compilation artifacts being stored in `crytic-export/`.
- **Default**: ""

#### `args`

- **Type**: [String]
- **Description**: Refers to any additional args that one may want to provide to `crytic-compile`. Run `crytic-compile --help`
  to view all of its supported flags. For example, if you would like to specify `--compile-force-framework foundry`, the
  `args` value will be `"args": ["--compile-force-framework", "foundry"]`.
  > 🚩 The `--export-format` and `--export-dir` are already used during compilation with `crytic-compile`.
  > Re-using these flags in `args` will cause the compilation to fail.
- **Default**: `[]`

#### Foundry projects built with Solar

Medusa supports Foundry projects compiled with [Solar](https://github.com/paradigmxyz/solar) (`solc = "solar"` in
`foundry.toml`). No configuration is needed.

A source map names each file by the id its compiler gave it. solc numbers a project's files in path order, the order
crytic-compile lists them in. Solar numbers them in another order, so medusa reads each file's id from:

1. the file's AST, which solc writes, and Solar writes from
   [paradigmxyz/solar#2344](https://github.com/paradigmxyz/solar/pull/2344) on;
2. otherwise, for a unit Foundry built with Solar, the `source_id_to_path` of its build-info in `<out>/build-info`,
   where `<out>` is the `--foundry-out-directory` given in `args`, or else the out directory of `forge config --json`;
3. otherwise, the file's position in crytic-compile's `sourceList`.

When a Solar build without ASTs matches no build-info, medusa logs a warning, since its coverage may then be reported on
the wrong files. Solar versions without ASTs also give medusa no contract kinds, so set `targetContracts` explicitly.

### `platformConfig` for `solc`

#### `target`

- **Type**: String
- **Description**: Refers to the target that is being compiled. The target must be a single `.sol` file.
