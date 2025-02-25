package wazero

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/engine/compiler"
	"github.com/tetratelabs/wazero/internal/engine/interpreter"
	"github.com/tetratelabs/wazero/internal/wasm"
)

// RuntimeConfig controls runtime behavior, with the default implementation as NewRuntimeConfig
//
// Ex. To explicitly limit to Wasm Core 1.0 features as opposed to relying on defaults:
//	rConfig = wazero.NewRuntimeConfig().WithWasmCore1()
//
// Note: RuntimeConfig is immutable. Each WithXXX function returns a new instance including the corresponding change.
type RuntimeConfig interface {

	// WithFeatureBulkMemoryOperations adds instructions modify ranges of memory or table entries
	// ("bulk-memory-operations"). This defaults to false as the feature was not finished in WebAssembly 1.0.
	//
	// Here are the notable effects:
	// * Adds `memory.fill`, `memory.init`, `memory.copy` and `data.drop` instructions.
	// * Adds `table.init`, `table.copy` and `elem.drop` instructions.
	// * Introduces a "passive" form of element and data segments.
	// * Stops checking "active" element and data segment boundaries at compile-time, meaning they can error at runtime.
	//
	// Note: "bulk-memory-operations" is mixed with the "reference-types" proposal
	// due to the WebAssembly Working Group merging them "mutually dependent".
	// Therefore, enabling this feature results in enabling WithFeatureReferenceTypes, and vice-versa.
	// See https://github.com/WebAssembly/spec/blob/main/proposals/bulk-memory-operations/Overview.md
	// See https://github.com/WebAssembly/spec/blob/main/proposals/reference-types/Overview.md
	// See https://github.com/WebAssembly/spec/pull/1287
	WithFeatureBulkMemoryOperations(bool) RuntimeConfig

	// WithFeatureMultiValue enables multiple values ("multi-value"). This defaults to false as the feature was not
	// finished in WebAssembly 1.0 (20191205).
	//
	// Here are the notable effects:
	// * Function (`func`) types allow more than one result
	// * Block types (`block`, `loop` and `if`) can be arbitrary function types
	//
	// See https://github.com/WebAssembly/spec/blob/main/proposals/multi-value/Overview.md
	WithFeatureMultiValue(bool) RuntimeConfig

	// WithFeatureMutableGlobal allows globals to be mutable. This defaults to true as the feature was finished in
	// WebAssembly 1.0 (20191205).
	//
	// When false, an api.Global can never be cast to an api.MutableGlobal, and any source that includes global vars
	// will fail to parse.
	WithFeatureMutableGlobal(bool) RuntimeConfig

	// WithFeatureNonTrappingFloatToIntConversion enables non-trapping float-to-int conversions.
	// ("nontrapping-float-to-int-conversion"). This defaults to false as the feature was not in WebAssembly 1.0.
	//
	// The only effect of enabling is allowing the following instructions, which return 0 on NaN instead of panicking.
	// * `i32.trunc_sat_f32_s`
	// * `i32.trunc_sat_f32_u`
	// * `i32.trunc_sat_f64_s`
	// * `i32.trunc_sat_f64_u`
	// * `i64.trunc_sat_f32_s`
	// * `i64.trunc_sat_f32_u`
	// * `i64.trunc_sat_f64_s`
	// * `i64.trunc_sat_f64_u`
	//
	// See https://github.com/WebAssembly/spec/blob/main/proposals/nontrapping-float-to-int-conversion/Overview.md
	WithFeatureNonTrappingFloatToIntConversion(bool) RuntimeConfig

	// WithFeatureReferenceTypes enables various instructions and features related to table and new reference types.
	//
	// * Introduction of new value types: `funcref` and `externref`.
	// * Support for the following new instructions:
	//   * `ref.null`
	//   * `ref.func`
	//   * `ref.is_null`
	//   * `table.fill`
	//   * `table.get`
	//   * `table.grow`
	//   * `table.set`
	//   * `table.size`
	// * Support for multiple tables per module:
	//   * `call_indirect`, `table.init`, `table.copy` and `elem.drop` instructions can take non-zero table index.
	//   * Element segments can take non-zero table index.
	//
	// Note: "reference-types" is mixed with the "bulk-memory-operations" proposal
	// due to the WebAssembly Working Group merging them "mutually dependent".
	// Therefore, enabling this feature results in enabling WithFeatureBulkMemoryOperations, and vice-versa.
	// See https://github.com/WebAssembly/spec/blob/main/proposals/bulk-memory-operations/Overview.md
	// See https://github.com/WebAssembly/spec/blob/main/proposals/reference-types/Overview.md
	// See https://github.com/WebAssembly/spec/pull/1287
	WithFeatureReferenceTypes(enabled bool) RuntimeConfig

	// WithFeatureSignExtensionOps enables sign extension instructions ("sign-extension-ops"). This defaults to false
	// as the feature was not in WebAssembly 1.0.
	//
	// Here are the notable effects:
	// * Adds instructions `i32.extend8_s`, `i32.extend16_s`, `i64.extend8_s`, `i64.extend16_s` and `i64.extend32_s`
	//
	// See https://github.com/WebAssembly/spec/blob/main/proposals/sign-extension-ops/Overview.md
	WithFeatureSignExtensionOps(bool) RuntimeConfig

	// WithFeatureSIMD enables the vector value type and vector instructions (aka SIMD).  This defaults to false
	// as the feature was not in WebAssembly 1.0.
	//
	// See https://github.com/WebAssembly/spec/blob/main/proposals/simd/SIMD.md
	WithFeatureSIMD(bool) RuntimeConfig

	// WithWasmCore1 enables features included in the WebAssembly Core Specification 1.0. Selecting this
	// overwrites any currently accumulated features with only those included in this W3C recommendation.
	//
	// This is default because as of mid 2022, this is the only version that is a Web Standard (W3C Recommendation).
	//
	// You can select the latest draft of the WebAssembly Core Specification 2.0 instead via WithWasmCore2. You can
	// also enable or disable individual features via `WithXXX` methods. Ex.
	//	rConfig = wazero.NewRuntimeConfig().WithWasmCore1().WithFeatureMutableGlobal(false)
	//
	// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/
	WithWasmCore1() RuntimeConfig

	// WithWasmCore2 enables features included in the WebAssembly Core Specification 2.0 (20220419). Selecting this
	// overwrites any currently accumulated features with only those included in this W3C working draft.
	//
	// This is not default because it is not yet incomplete and also not yet a Web Standard (W3C Recommendation).
	//
	// Even after selecting this, you can enable or disable individual features via `WithXXX` methods. Ex.
	//	rConfig = wazero.NewRuntimeConfig().WithWasmCore2().WithFeatureMutableGlobal(false)
	//
	// See https://www.w3.org/TR/2022/WD-wasm-core-2-20220419/
	WithWasmCore2() RuntimeConfig
}

type runtimeConfig struct {
	enabledFeatures wasm.Features
	newEngine       func(wasm.Features) wasm.Engine
}

// engineLessConfig helps avoid copy/pasting the wrong defaults.
var engineLessConfig = &runtimeConfig{
	enabledFeatures: wasm.Features20191205,
}

// NewRuntimeConfigCompiler compiles WebAssembly modules into
// runtime.GOARCH-specific assembly for optimal performance.
//
// The default implementation is AOT (Ahead of Time) compilation, applied at
// Runtime.CompileModule. This allows consistent runtime performance, as well
// the ability to reduce any first request penalty.
//
// Note: While this is technically AOT, this does not imply any action on your
// part. wazero automatically performs ahead-of-time compilation as needed when
// Runtime.CompileModule is invoked.
// Note: This panics at runtime the runtime.GOOS or runtime.GOARCH does not
// support Compiler. Use NewRuntimeConfig to safely detect and fallback to
// NewRuntimeConfigInterpreter if needed.
func NewRuntimeConfigCompiler() RuntimeConfig {
	ret := *engineLessConfig // copy
	ret.newEngine = compiler.NewEngine
	return &ret
}

// NewRuntimeConfigInterpreter interprets WebAssembly modules instead of compiling them into assembly.
func NewRuntimeConfigInterpreter() RuntimeConfig {
	ret := *engineLessConfig // copy
	ret.newEngine = interpreter.NewEngine
	return &ret
}

// WithFeatureBulkMemoryOperations implements RuntimeConfig.WithFeatureBulkMemoryOperations
func (c *runtimeConfig) WithFeatureBulkMemoryOperations(enabled bool) RuntimeConfig {
	ret := *c // copy
	ret.enabledFeatures = ret.enabledFeatures.Set(wasm.FeatureBulkMemoryOperations, enabled)
	// bulk-memory-operations proposal is mutually-dependant with reference-types proposal.
	ret.enabledFeatures = ret.enabledFeatures.Set(wasm.FeatureReferenceTypes, enabled)
	return &ret
}

// WithFeatureMultiValue implements RuntimeConfig.WithFeatureMultiValue
func (c *runtimeConfig) WithFeatureMultiValue(enabled bool) RuntimeConfig {
	ret := *c // copy
	ret.enabledFeatures = ret.enabledFeatures.Set(wasm.FeatureMultiValue, enabled)
	return &ret
}

// WithFeatureMutableGlobal implements RuntimeConfig.WithFeatureMutableGlobal
func (c *runtimeConfig) WithFeatureMutableGlobal(enabled bool) RuntimeConfig {
	ret := *c // copy
	ret.enabledFeatures = ret.enabledFeatures.Set(wasm.FeatureMutableGlobal, enabled)
	return &ret
}

// WithFeatureNonTrappingFloatToIntConversion implements RuntimeConfig.WithFeatureNonTrappingFloatToIntConversion
func (c *runtimeConfig) WithFeatureNonTrappingFloatToIntConversion(enabled bool) RuntimeConfig {
	ret := *c // copy
	ret.enabledFeatures = ret.enabledFeatures.Set(wasm.FeatureNonTrappingFloatToIntConversion, enabled)
	return &ret
}

// WithFeatureReferenceTypes implements RuntimeConfig.WithFeatureReferenceTypes
func (c *runtimeConfig) WithFeatureReferenceTypes(enabled bool) RuntimeConfig {
	ret := *c // copy
	ret.enabledFeatures = ret.enabledFeatures.Set(wasm.FeatureReferenceTypes, enabled)
	// reference-types proposal is mutually-dependant with bulk-memory-operations proposal.
	ret.enabledFeatures = ret.enabledFeatures.Set(wasm.FeatureBulkMemoryOperations, enabled)
	return &ret
}

// WithFeatureSignExtensionOps implements RuntimeConfig.WithFeatureSignExtensionOps
func (c *runtimeConfig) WithFeatureSignExtensionOps(enabled bool) RuntimeConfig {
	ret := *c // copy
	ret.enabledFeatures = ret.enabledFeatures.Set(wasm.FeatureSignExtensionOps, enabled)
	return &ret
}

// WithFeatureSIMD implements RuntimeConfig.WithFeatureSIMD
func (c *runtimeConfig) WithFeatureSIMD(enabled bool) RuntimeConfig {
	ret := *c // copy
	ret.enabledFeatures = ret.enabledFeatures.Set(wasm.FeatureSIMD, enabled)
	return &ret
}

// WithWasmCore1 implements RuntimeConfig.WithWasmCore1
func (c *runtimeConfig) WithWasmCore1() RuntimeConfig {
	ret := *c // copy
	ret.enabledFeatures = wasm.Features20191205
	return &ret
}

// WithWasmCore2 implements RuntimeConfig.WithWasmCore2
func (c *runtimeConfig) WithWasmCore2() RuntimeConfig {
	ret := *c // copy
	ret.enabledFeatures = wasm.Features20220419
	return &ret
}

// CompiledModule is a WebAssembly 1.0 module ready to be instantiated (Runtime.InstantiateModule) as an api.Module.
//
// Note: Closing the wazero.Runtime closes any CompiledModule it compiled.
// Note: In WebAssembly language, this is a decoded, validated, and possibly also compiled module. wazero avoids using
// the name "Module" for both before and after instantiation as the name conflation has caused confusion.
// See https://www.w3.org/TR/2019/REC-wasm-core-1-20191205/#semantic-phases%E2%91%A0
type CompiledModule interface {
	// Close releases all the allocated resources for this CompiledModule.
	//
	// Note: It is safe to call Close while having outstanding calls from an api.Module instantiated from this.
	Close(context.Context) error
}

type compiledCode struct {
	module *wasm.Module
	// compiledEngine holds an engine on which `module` is compiled.
	compiledEngine wasm.Engine
}

// Close implements CompiledModule.Close
func (c *compiledCode) Close(_ context.Context) error {
	// Note: If you use the context.Context param, don't forget to coerce nil to context.Background()!

	c.compiledEngine.DeleteCompiledModule(c.module)
	// It is possible the underlying may need to return an error later, but in any case this matches api.Module.Close.
	return nil
}

// CompileConfig allows you to override what was decoded from source, prior to compilation (ModuleBuilder.Compile or
// Runtime.CompileModule).
//
// For example, WithImportRenamer allows you to override hard-coded names that don't match your requirements.
//
// Note: CompileConfig is immutable. Each WithXXX function returns a new instance including the corresponding change.
type CompileConfig interface {

	// WithImportRenamer can rename imports or break them into different modules. No default.
	//
	// Note: A nil function is invalid and ignored.
	// Note: This is currently not relevant for ModuleBuilder as it has no means to define imports.
	WithImportRenamer(api.ImportRenamer) CompileConfig

	// WithMemorySizer are the allocation parameters used for a Wasm memory.
	// The default is to set cap=min and max=65536 if unset.
	//
	// Note: A nil function is invalid and ignored.
	WithMemorySizer(api.MemorySizer) CompileConfig
}

type compileConfig struct {
	importRenamer api.ImportRenamer
	memorySizer   api.MemorySizer
}

func NewCompileConfig() CompileConfig {
	return &compileConfig{
		importRenamer: nil,
		memorySizer:   wasm.MemorySizer,
	}
}

// WithImportRenamer implements CompileConfig.WithImportRenamer
func (c *compileConfig) WithImportRenamer(importRenamer api.ImportRenamer) CompileConfig {
	if importRenamer == nil {
		return c
	}
	ret := *c // copy
	ret.importRenamer = importRenamer
	return &ret
}

// WithMemorySizer implements CompileConfig.WithMemorySizer
func (c *compileConfig) WithMemorySizer(memorySizer api.MemorySizer) CompileConfig {
	if memorySizer == nil {
		return c
	}
	ret := *c // copy
	ret.memorySizer = memorySizer
	return &ret
}

// ModuleConfig configures resources needed by functions that have low-level interactions with the host operating
// system. Using this, resources such as STDIN can be isolated, so that the same module can be safely instantiated
// multiple times.
//
// Note: While wazero supports Windows as a platform, host functions using ModuleConfig follow a UNIX dialect.
// See RATIONALE.md for design background and relationship to WebAssembly System Interfaces (WASI).
//
// Note: ModuleConfig is immutable. Each WithXXX function returns a new instance including the corresponding change.
type ModuleConfig interface {

	// WithArgs assigns command-line arguments visible to an imported function that reads an arg vector (argv). Defaults to
	// none.
	//
	// These values are commonly read by the functions like "args_get" in "wasi_snapshot_preview1" although they could be
	// read by functions imported from other modules.
	//
	// Similar to os.Args and exec.Cmd Env, many implementations would expect a program name to be argv[0]. However, neither
	// WebAssembly nor WebAssembly System Interfaces (WASI) define this. Regardless, you may choose to set the first
	// argument to the same value set via WithName.
	//
	// Note: This does not default to os.Args as that violates sandboxing.
	// Note: Runtime.InstantiateModule errs if any value is empty.
	// See https://linux.die.net/man/3/argv
	// See https://en.wikipedia.org/wiki/Null-terminated_string
	WithArgs(...string) ModuleConfig

	// WithEnv sets an environment variable visible to a Module that imports functions. Defaults to none.
	//
	// Validation is the same as os.Setenv on Linux and replaces any existing value. Unlike exec.Cmd Env, this does not
	// default to the current process environment as that would violate sandboxing. This also does not preserve order.
	//
	// Environment variables are commonly read by the functions like "environ_get" in "wasi_snapshot_preview1" although
	// they could be read by functions imported from other modules.
	//
	// While similar to process configuration, there are no assumptions that can be made about anything OS-specific. For
	// example, neither WebAssembly nor WebAssembly System Interfaces (WASI) define concerns processes have, such as
	// case-sensitivity on environment keys. For portability, define entries with case-insensitively unique keys.
	//
	// Note: Runtime.InstantiateModule errs if the key is empty or contains a NULL(0) or equals("") character.
	// See https://linux.die.net/man/3/environ
	// See https://en.wikipedia.org/wiki/Null-terminated_string
	WithEnv(key, value string) ModuleConfig

	// WithFS assigns the file system to use for any paths beginning at "/". Defaults to not found.
	//
	// Ex. This sets a read-only, embedded file-system to serve files under the root ("/") and working (".") directories:
	//
	//	//go:embed testdata/index.html
	//	var testdataIndex embed.FS
	//
	//	rooted, err := fs.Sub(testdataIndex, "testdata")
	//	require.NoError(t, err)
	//
	//	// "index.html" is accessible as both "/index.html" and "./index.html" because we didn't use WithWorkDirFS.
	//	config := wazero.NewModuleConfig().WithFS(rooted)
	//
	// Note: This sets WithWorkDirFS to the same file-system unless already set.
	WithFS(fs.FS) ModuleConfig

	// WithName configures the module name. Defaults to what was decoded or overridden via CompileConfig.WithModuleName.
	WithName(string) ModuleConfig

	// WithStartFunctions configures the functions to call after the module is instantiated. Defaults to "_start".
	//
	// Note: If any function doesn't exist, it is skipped. However, all functions that do exist are called in order.
	WithStartFunctions(...string) ModuleConfig

	// WithStderr configures where standard error (file descriptor 2) is written. Defaults to io.Discard.
	//
	// This writer is most commonly used by the functions like "fd_write" in "wasi_snapshot_preview1" although it could
	// be used by functions imported from other modules.
	//
	// Note: The caller is responsible to close any io.Writer they supply: It is not closed on api.Module Close.
	// Note: This does not default to os.Stderr as that both violates sandboxing and prevents concurrent modules.
	// See https://linux.die.net/man/3/stderr
	WithStderr(io.Writer) ModuleConfig

	// WithStdin configures where standard input (file descriptor 0) is read. Defaults to return io.EOF.
	//
	// This reader is most commonly used by the functions like "fd_read" in "wasi_snapshot_preview1" although it could
	// be used by functions imported from other modules.
	//
	// Note: The caller is responsible to close any io.Reader they supply: It is not closed on api.Module Close.
	// Note: This does not default to os.Stdin as that both violates sandboxing and prevents concurrent modules.
	// See https://linux.die.net/man/3/stdin
	WithStdin(io.Reader) ModuleConfig

	// WithStdout configures where standard output (file descriptor 1) is written. Defaults to io.Discard.
	//
	// This writer is most commonly used by the functions like "fd_write" in "wasi_snapshot_preview1" although it could
	// be used by functions imported from other modules.
	//
	// Note: The caller is responsible to close any io.Writer they supply: It is not closed on api.Module Close.
	// Note: This does not default to os.Stdout as that both violates sandboxing and prevents concurrent modules.
	// See https://linux.die.net/man/3/stdout
	WithStdout(io.Writer) ModuleConfig

	// WithWorkDirFS indicates the file system to use for any paths beginning at "./". Defaults to the same as WithFS.
	//
	// Ex. This sets a read-only, embedded file-system as the root ("/"), and a mutable one as the working directory ("."):
	//
	//	//go:embed appA
	//	var rootFS embed.FS
	//
	//	// Files relative to this source under appA are available under "/" and files relative to "/work/appA" under ".".
	//	config := wazero.NewModuleConfig().WithFS(rootFS).WithWorkDirFS(os.DirFS("/work/appA"))
	//
	// Note: os.DirFS documentation includes important notes about isolation, which also applies to fs.Sub. As of Go 1.18,
	// the built-in file-systems are not jailed (chroot). See https://github.com/golang/go/issues/42322
	WithWorkDirFS(fs.FS) ModuleConfig
}

type moduleConfig struct {
	name           string
	startFunctions []string
	stdin          io.Reader
	stdout         io.Writer
	stderr         io.Writer
	args           []string
	// environ is pair-indexed to retain order similar to os.Environ.
	environ []string
	// environKeys allow overwriting of existing values.
	environKeys map[string]int

	// preopenFD has the next FD number to use
	preopenFD uint32
	// preopens are keyed on file descriptor and only include the Path and FS fields.
	preopens map[uint32]*wasm.FileEntry
	// preopenPaths allow overwriting of existing paths.
	preopenPaths map[string]uint32
}

func NewModuleConfig() ModuleConfig {
	return &moduleConfig{
		startFunctions: []string{"_start"},
		environKeys:    map[string]int{},
		preopenFD:      uint32(3), // after stdin/stdout/stderr
		preopens:       map[uint32]*wasm.FileEntry{},
		preopenPaths:   map[string]uint32{},
	}
}

// WithArgs implements ModuleConfig.WithArgs
func (c *moduleConfig) WithArgs(args ...string) ModuleConfig {
	ret := *c // copy
	ret.args = args
	return &ret
}

// WithEnv implements ModuleConfig.WithEnv
func (c *moduleConfig) WithEnv(key, value string) ModuleConfig {
	ret := *c // copy
	// Check to see if this key already exists and update it.
	if i, ok := ret.environKeys[key]; ok {
		ret.environ[i+1] = value // environ is pair-indexed, so the value is 1 after the key.
	} else {
		ret.environKeys[key] = len(ret.environ)
		ret.environ = append(ret.environ, key, value)
	}
	return &ret
}

// WithFS implements ModuleConfig.WithFS
func (c *moduleConfig) WithFS(fs fs.FS) ModuleConfig {
	ret := *c // copy
	ret.setFS("/", fs)
	return &ret
}

// WithName implements ModuleConfig.WithName
func (c *moduleConfig) WithName(name string) ModuleConfig {
	ret := *c // copy
	ret.name = name
	return &ret
}

// WithStartFunctions implements ModuleConfig.WithStartFunctions
func (c *moduleConfig) WithStartFunctions(startFunctions ...string) ModuleConfig {
	ret := *c // copy
	ret.startFunctions = startFunctions
	return &ret
}

// WithStderr implements ModuleConfig.WithStderr
func (c *moduleConfig) WithStderr(stderr io.Writer) ModuleConfig {
	ret := *c // copy
	ret.stderr = stderr
	return &ret
}

// WithStdin implements ModuleConfig.WithStdin
func (c *moduleConfig) WithStdin(stdin io.Reader) ModuleConfig {
	ret := *c // copy
	ret.stdin = stdin
	return &ret
}

// WithStdout implements ModuleConfig.WithStdout
func (c *moduleConfig) WithStdout(stdout io.Writer) ModuleConfig {
	ret := *c // copy
	ret.stdout = stdout
	return &ret
}

// WithWorkDirFS implements ModuleConfig.WithWorkDirFS
func (c *moduleConfig) WithWorkDirFS(fs fs.FS) ModuleConfig {
	ret := *c // copy
	ret.setFS(".", fs)
	return &ret
}

// setFS maps a path to a file-system. This is only used for base paths: "/" and ".".
func (c *moduleConfig) setFS(path string, fs fs.FS) {
	// Check to see if this key already exists and update it.
	entry := &wasm.FileEntry{Path: path, FS: fs}
	if fd, ok := c.preopenPaths[path]; ok {
		c.preopens[fd] = entry
	} else {
		c.preopens[c.preopenFD] = entry
		c.preopenPaths[path] = c.preopenFD
		c.preopenFD++
	}
}

// toSysContext creates a baseline wasm.SysContext configured by ModuleConfig.
func (c *moduleConfig) toSysContext() (sys *wasm.SysContext, err error) {
	var environ []string // Intentionally doesn't pre-allocate to reduce logic to default to nil.
	// Same validation as syscall.Setenv for Linux
	for i := 0; i < len(c.environ); i += 2 {
		key, value := c.environ[i], c.environ[i+1]
		if len(key) == 0 {
			err = errors.New("environ invalid: empty key")
			return
		}
		for j := 0; j < len(key); j++ {
			if key[j] == '=' { // NUL enforced in NewSysContext
				err = errors.New("environ invalid: key contains '=' character")
				return
			}
		}
		environ = append(environ, key+"="+value)
	}

	// Ensure no-one set a nil FD. We do this here instead of at the call site to allow chaining as nil is unexpected.
	rootFD := uint32(0) // zero is invalid
	setWorkDirFS := false
	preopens := c.preopens
	for fd, entry := range preopens {
		if entry.FS == nil {
			err = fmt.Errorf("FS for %s is nil", entry.Path)
			return
		} else if entry.Path == "/" {
			rootFD = fd
		} else if entry.Path == "." {
			setWorkDirFS = true
		}
	}

	// Default the working directory to the root FS if it exists.
	if rootFD != 0 && !setWorkDirFS {
		preopens[c.preopenFD] = &wasm.FileEntry{Path: ".", FS: preopens[rootFD].FS}
	}

	return wasm.NewSysContext(math.MaxUint32, c.args, environ, c.stdin, c.stdout, c.stderr, preopens)
}
