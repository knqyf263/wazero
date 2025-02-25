package wazero

import (
	"context"
	_ "embed"
	"fmt"
	"math"
	"testing"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/internal/leb128"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wasm/binary"
	"github.com/tetratelabs/wazero/sys"
)

// testCtx is an arbitrary, non-default context. Non-nil also prevents linter errors.
var testCtx = context.WithValue(context.Background(), struct{}{}, "arbitrary")

func TestNewRuntimeWithConfig_PanicsOnWrongImpl(t *testing.T) {
	// It causes maintenance to define an impl of RuntimeConfig in tests just to verify the error when it is wrong.
	// Instead, we pass nil which is implicitly the wrong type, as that's less work!
	err := require.CapturePanic(func() {
		NewRuntimeWithConfig(nil)
	})

	require.EqualError(t, err, "unsupported wazero.RuntimeConfig implementation: <nil>")
}

func TestRuntime_CompileModule(t *testing.T) {
	tests := []struct {
		name         string
		runtime      Runtime
		source       []byte
		expectedName string
	}{
		{
			name:   "text no name",
			source: []byte(`(module)`),
		},
		{
			name:   "text empty name",
			source: []byte(`(module $)`),
		},
		{
			name:         "text name",
			source:       []byte(`(module $test)`),
			expectedName: "test",
		},
		{
			name:   "binary no name section",
			source: binary.EncodeModule(&wasm.Module{}),
		},
		{
			name:   "binary empty NameSection.ModuleName",
			source: binary.EncodeModule(&wasm.Module{NameSection: &wasm.NameSection{}}),
		},
		{
			name:         "binary NameSection.ModuleName",
			source:       binary.EncodeModule(&wasm.Module{NameSection: &wasm.NameSection{ModuleName: "test"}}),
			expectedName: "test",
		},
	}

	r := NewRuntime()
	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			m, err := r.CompileModule(testCtx, tc.source, NewCompileConfig())
			require.NoError(t, err)
			code := m.(*compiledCode)
			defer code.Close(testCtx)
			if tc.expectedName != "" {
				require.Equal(t, tc.expectedName, code.module.NameSection.ModuleName)
			}
			require.Equal(t, r.(*runtime).store.Engine, code.compiledEngine)
		})
	}

	t.Run("WithMemorySizer", func(t *testing.T) {
		source := []byte(`(module (memory 1))`)

		m, err := r.CompileModule(testCtx, source, NewCompileConfig().
			WithMemorySizer(func(minPages uint32, maxPages *uint32) (min, capacity, max uint32) {
				return 1, 2, 3
			}))
		require.NoError(t, err)
		code := m.(*compiledCode)
		defer code.Close(testCtx)

		require.Equal(t, &wasm.Memory{
			Min: 1,
			Cap: 2,
			Max: 3,
		}, code.module.MemorySection)
	})

	t.Run("WithImportReplacements", func(t *testing.T) {
		source := []byte(`(module
  (import "js" "increment" (func $increment (result i32)))
  (import "js" "decrement" (func $decrement (result i32)))
  (import "js" "wasm_increment" (func $wasm_increment (result i32)))
  (import "js" "wasm_decrement" (func $wasm_decrement (result i32)))
)`)

		m, err := r.CompileModule(testCtx, source, NewCompileConfig().
			WithImportRenamer(func(externType api.ExternType, oldModule, oldName string) (string, string) {
				if externType != api.ExternTypeFunc {
					return oldModule, oldName
				}
				switch oldName {
				case "increment", "decrement":
					return "go", oldName
				case "wasm_increment", "wasm_decrement":
					return "wasm", oldName
				default:
					return oldModule, oldName
				}
			}))
		require.NoError(t, err)
		code := m.(*compiledCode)
		defer code.Close(testCtx)

		require.Equal(t, []*wasm.Import{
			{
				Module: "go", Name: "increment",
				Type:     wasm.ExternTypeFunc,
				DescFunc: 0,
			},
			{
				Module: "go", Name: "decrement",
				Type:     wasm.ExternTypeFunc,
				DescFunc: 0,
			},
			{
				Module: "wasm", Name: "wasm_increment",
				Type:     wasm.ExternTypeFunc,
				DescFunc: 0,
			},
			{
				Module: "wasm", Name: "wasm_decrement",
				Type:     wasm.ExternTypeFunc,
				DescFunc: 0,
			},
		}, code.module.ImportSection)
	})
}

func TestRuntime_CompileModule_Errors(t *testing.T) {
	tests := []struct {
		name        string
		config      CompileConfig
		source      []byte
		expectedErr string
	}{
		{
			name:        "nil",
			expectedErr: "source == nil",
		},
		{
			name:        "invalid binary",
			source:      append(binary.Magic, []byte("yolo")...),
			expectedErr: "invalid version header",
		},
		{
			name:        "invalid text",
			source:      []byte(`(modular)`),
			expectedErr: "1:2: unexpected field: modular",
		},
		{
			name:        "memory has too many pages text",
			source:      []byte(`(module (memory 70000))`),
			expectedErr: "1:17: min 70000 pages (4 Gi) over limit of 65536 pages (4 Gi) in module.memory[0]",
		},
		{
			name: "memory cap < min", // only one test to avoid duplicating tests in module_test.go
			config: NewCompileConfig().WithMemorySizer(func(minPages uint32, maxPages *uint32) (min, capacity, max uint32) {
				return 3, 1, 3
			}),
			source:      []byte(`(module (memory 3))`),
			expectedErr: "1:17: capacity 1 pages (64 Ki) less than minimum 3 pages (192 Ki) in module.memory[0]",
		},
		{
			name: "memory cap < min exported", // only one test to avoid duplicating tests in module_test.go
			config: NewCompileConfig().WithMemorySizer(func(minPages uint32, maxPages *uint32) (min, capacity, max uint32) {
				return 3, 2, 3
			}),
			source:      []byte(`(module (memory 3) (export "memory" (memory 0)))`),
			expectedErr: "1:17: capacity 2 pages (128 Ki) less than minimum 3 pages (192 Ki) in module.memory[0]",
		},
		{
			name:        "memory has too many pages binary",
			source:      binary.EncodeModule(&wasm.Module{MemorySection: &wasm.Memory{Min: 2, Cap: 2, Max: 70000, IsMaxEncoded: true}}),
			expectedErr: "section memory: max 70000 pages (4 Gi) over limit of 65536 pages (4 Gi)",
		},
	}

	r := NewRuntime()
	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			config := tc.config
			if config == nil {
				config = NewCompileConfig()
			}
			_, err := r.CompileModule(testCtx, tc.source, config)
			require.EqualError(t, err, tc.expectedErr)
		})
	}
}

// TestModule_Memory only covers a couple cases to avoid duplication of internal/wasm/runtime_test.go
func TestModule_Memory(t *testing.T) {
	tests := []struct {
		name        string
		builder     func(Runtime) ModuleBuilder
		expected    bool
		expectedLen uint32
	}{
		{
			name: "no memory",
			builder: func(r Runtime) ModuleBuilder {
				return r.NewModuleBuilder(t.Name())
			},
		},
		{
			name: "memory exported, one page",
			builder: func(r Runtime) ModuleBuilder {
				return r.NewModuleBuilder(t.Name()).ExportMemory("memory", 1)
			},
			expected:    true,
			expectedLen: 65536,
		},
	}

	for _, tt := range tests {
		tc := tt

		r := NewRuntime()
		t.Run(tc.name, func(t *testing.T) {
			// Instantiate the module and get the export of the above memory
			module, err := tc.builder(r).Instantiate(testCtx)
			require.NoError(t, err)
			defer module.Close(testCtx)

			mem := module.ExportedMemory("memory")
			if tc.expected {
				require.Equal(t, tc.expectedLen, mem.Size(testCtx))
			} else {
				require.Nil(t, mem)
			}
		})
	}
}

// TestModule_Global only covers a couple cases to avoid duplication of internal/wasm/global_test.go
func TestModule_Global(t *testing.T) {
	globalVal := int64(100) // intentionally a value that differs in signed vs unsigned encoding

	tests := []struct {
		name                      string
		module                    *wasm.Module // module as wat doesn't yet support globals
		builder                   func(Runtime) ModuleBuilder
		expected, expectedMutable bool
	}{
		{
			name:   "no global",
			module: &wasm.Module{},
		},
		{
			name: "global not exported",
			module: &wasm.Module{
				GlobalSection: []*wasm.Global{
					{
						Type: &wasm.GlobalType{ValType: wasm.ValueTypeI64, Mutable: true},
						Init: &wasm.ConstantExpression{Opcode: wasm.OpcodeI64Const, Data: leb128.EncodeInt64(globalVal)},
					},
				},
			},
		},
		{
			name: "global exported",
			builder: func(r Runtime) ModuleBuilder {
				return r.NewModuleBuilder(t.Name()).ExportGlobalI64("global", globalVal)
			},
			expected: true,
		},
		{
			name: "global exported and mutable",
			module: &wasm.Module{
				GlobalSection: []*wasm.Global{
					{
						Type: &wasm.GlobalType{ValType: wasm.ValueTypeI64, Mutable: true},
						Init: &wasm.ConstantExpression{Opcode: wasm.OpcodeI64Const, Data: leb128.EncodeInt64(globalVal)},
					},
				},
				ExportSection: []*wasm.Export{
					{Type: wasm.ExternTypeGlobal, Name: "global"},
				},
			},
			expected:        true,
			expectedMutable: true,
		},
	}

	for _, tt := range tests {
		tc := tt

		r := NewRuntime().(*runtime)
		t.Run(tc.name, func(t *testing.T) {
			var m CompiledModule
			if tc.module != nil {
				m = &compiledCode{module: tc.module}
			} else {
				m, _ = tc.builder(r).Compile(testCtx, NewCompileConfig())
			}
			code := m.(*compiledCode)

			err := r.store.Engine.CompileModule(testCtx, code.module)
			require.NoError(t, err)

			// Instantiate the module and get the export of the above global
			module, err := r.InstantiateModule(testCtx, code, NewModuleConfig())
			require.NoError(t, err)
			defer module.Close(testCtx)

			global := module.ExportedGlobal("global")
			if !tc.expected {
				require.Nil(t, global)
				return
			}
			require.Equal(t, uint64(globalVal), global.Get(testCtx))

			mutable, ok := global.(api.MutableGlobal)
			require.Equal(t, tc.expectedMutable, ok)
			if ok {
				mutable.Set(testCtx, 2)
				require.Equal(t, uint64(2), global.Get(testCtx))
			}
		})
	}
}

func TestFunction_Context(t *testing.T) {
	tests := []struct {
		name     string
		ctx      context.Context
		expected context.Context
	}{
		{
			name:     "nil defaults to context.Background",
			ctx:      nil,
			expected: context.Background(),
		},
		{
			name:     "set context",
			ctx:      testCtx,
			expected: testCtx,
		},
	}

	for _, tt := range tests {
		tc := tt

		t.Run(tc.name, func(t *testing.T) {
			r := NewRuntime()

			// Define a host function so that we can catch the context propagated from a module function call
			functionName := "fn"
			expectedResult := uint64(math.MaxUint64)
			hostFn := func(ctx context.Context) uint64 {
				require.Equal(t, tc.expected, ctx)
				return expectedResult
			}
			source, closer := requireImportAndExportFunction(t, r, hostFn, functionName)
			defer closer(testCtx) // nolint

			// Instantiate the module and get the export of the above hostFn
			module, err := r.InstantiateModuleFromCode(tc.ctx, source)
			require.NoError(t, err)
			defer module.Close(testCtx)

			// This fails if the function wasn't invoked, or had an unexpected context.
			results, err := module.ExportedFunction(functionName).Call(tc.ctx)
			require.NoError(t, err)
			require.Equal(t, expectedResult, results[0])
		})
	}
}

func TestRuntime_InstantiateModule_UsesContext(t *testing.T) {
	r := NewRuntime()

	// Define a function that will be set as the start function
	var calledStart bool
	start := func(ctx context.Context) {
		calledStart = true
		require.Equal(t, testCtx, ctx)
	}

	env, err := r.NewModuleBuilder("env").
		ExportFunction("start", start).
		Instantiate(testCtx)
	require.NoError(t, err)
	defer env.Close(testCtx)

	code, err := r.CompileModule(testCtx, []byte(`(module $runtime_test.go
	(import "env" "start" (func $start))
	(start $start)
)`), NewCompileConfig())
	require.NoError(t, err)
	defer code.Close(testCtx)

	// Instantiate the module, which calls the start function. This will fail if the context wasn't as intended.
	m, err := r.InstantiateModule(testCtx, code, NewModuleConfig())
	require.NoError(t, err)
	defer m.Close(testCtx)

	require.True(t, calledStart)
}

// TestInstantiateModuleFromCode_DoesntEnforce_Start ensures wapc-go work when modules import WASI, but don't export "_start".
func TestInstantiateModuleFromCode_DoesntEnforce_Start(t *testing.T) {
	r := NewRuntime()

	mod, err := r.InstantiateModuleFromCode(testCtx, []byte(`(module $wasi_test.go
	(memory 1)
	(export "memory" (memory 0))
)`))
	require.NoError(t, err)
	require.NoError(t, mod.Close(testCtx))
}

func TestRuntime_InstantiateModuleFromCode_UsesContext(t *testing.T) {
	r := NewRuntime()

	// Define a function that will be re-exported as the WASI function: _start
	var calledStart bool
	start := func(ctx context.Context) {
		calledStart = true
		require.Equal(t, testCtx, ctx)
	}

	host, err := r.NewModuleBuilder("").
		ExportFunction("start", start).
		Instantiate(testCtx)
	require.NoError(t, err)
	defer host.Close(testCtx)

	// Start the module as a WASI command. This will fail if the context wasn't as intended.
	mod, err := r.InstantiateModuleFromCode(testCtx, []byte(`(module $start
	(import "" "start" (func $start))
	(memory 1)
	(export "_start" (func $start))
	(export "memory" (memory 0))
)`))
	require.NoError(t, err)
	defer mod.Close(testCtx)

	require.True(t, calledStart)
}

func TestInstantiateModule_PanicsOnWrongCompiledCodeImpl(t *testing.T) {
	// It causes maintenance to define an impl of CompiledModule in tests just to verify the error when it is wrong.
	// Instead, we pass nil which is implicitly the wrong type, as that's less work!
	r := NewRuntime()
	err := require.CapturePanic(func() {
		_, _ = r.InstantiateModule(testCtx, nil, NewModuleConfig())
	})

	require.EqualError(t, err, "unsupported wazero.CompiledModule implementation: <nil>")
}

func TestInstantiateModule_PanicsOnWrongModuleConfigImpl(t *testing.T) {
	r := NewRuntime()
	code, err := r.CompileModule(testCtx, []byte(`(module)`), NewCompileConfig())
	require.NoError(t, err)
	defer code.Close(testCtx)

	// It causes maintenance to define an impl of ModuleConfig in tests just to verify the error when it is wrong.
	// Instead, we pass nil which is implicitly the wrong type, as that's less work!
	err = require.CapturePanic(func() {
		_, _ = r.InstantiateModule(testCtx, code, nil)
	})

	require.EqualError(t, err, "unsupported wazero.ModuleConfig implementation: <nil>")
}

// TestInstantiateModule_WithName tests that we can pre-validate (cache) a module and instantiate it under
// different names. This pattern is used in wapc-go.
func TestInstantiateModule_WithName(t *testing.T) {
	r := NewRuntime()
	base, err := r.CompileModule(testCtx, []byte(`(module $0 (memory 1))`), NewCompileConfig())
	require.NoError(t, err)
	defer base.Close(testCtx)

	require.Equal(t, "0", base.(*compiledCode).module.NameSection.ModuleName)

	// Use the same runtime to instantiate multiple modules
	internal := r.(*runtime).store
	m1, err := r.InstantiateModule(testCtx, base, NewModuleConfig().WithName("1"))
	require.NoError(t, err)
	defer m1.Close(testCtx)

	require.Nil(t, internal.Module("0"))
	require.Equal(t, internal.Module("1"), m1)

	m2, err := r.InstantiateModule(testCtx, base, NewModuleConfig().WithName("2"))
	require.NoError(t, err)
	defer m2.Close(testCtx)

	require.Nil(t, internal.Module("0"))
	require.Equal(t, internal.Module("2"), m2)
}

func TestInstantiateModule_ExitError(t *testing.T) {
	r := NewRuntime()

	start := func(ctx context.Context, m api.Module) {
		require.NoError(t, m.CloseWithExitCode(ctx, 2))
	}

	_, err := r.NewModuleBuilder("env").ExportFunction("_start", start).Instantiate(testCtx)

	// Ensure the exit error propagated and didn't wrap.
	require.Equal(t, err, sys.NewExitError("env", 2))
}

func TestClose(t *testing.T) {
	for _, tc := range []struct {
		name     string
		exitCode uint32
	}{
		{
			name:     "exit code 0",
			exitCode: uint32(0),
		},
		{
			name:     "exit code 2",
			exitCode: uint32(2),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRuntime()

			m1, err := r.NewModuleBuilder("mod1").ExportFunction("func1", func() {}).Instantiate(testCtx)
			require.NoError(t, err)
			m2, err := r.NewModuleBuilder("mod2").ExportFunction("func2", func() {}).Instantiate(testCtx)
			require.NoError(t, err)

			func1 := m1.ExportedFunction("func1")
			func2 := m2.ExportedFunction("func2")

			// Modules not closed so calls succeed

			_, err = func1.Call(testCtx)
			require.NoError(t, err)

			_, err = func2.Call(testCtx)
			require.NoError(t, err)

			if tc.exitCode == 0 {
				err = r.Close(testCtx)
			} else {
				err = r.CloseWithExitCode(testCtx, tc.exitCode)
			}
			require.NoError(t, err)

			// Modules closed so calls fail
			_, err = func1.Call(testCtx)
			require.ErrorIs(t, err, sys.NewExitError("mod1", tc.exitCode))

			_, err = func2.Call(testCtx)
			require.ErrorIs(t, err, sys.NewExitError("mod2", tc.exitCode))
		})
	}
}

func TestClose_ClosesCompiledModules(t *testing.T) {
	engine := &mockEngine{name: "mock", cachedModules: map[*wasm.Module]struct{}{}}
	conf := *engineLessConfig
	conf.newEngine = func(_ wasm.Features) wasm.Engine {
		return engine
	}
	r := NewRuntimeWithConfig(&conf)
	// Normally compiled modules are closed when instantiated but this is never instantiated.
	_, err := r.CompileModule(testCtx, []byte(`(module $0 (memory 1))`), NewCompileConfig())
	require.NoError(t, err)
	require.Equal(t, 1, len(engine.cachedModules))

	err = r.Close(testCtx)
	require.NoError(t, err)
	require.Equal(t, 0, len(engine.cachedModules))
}

// requireImportAndExportFunction re-exports a host function because only host functions can see the propagated context.
func requireImportAndExportFunction(t *testing.T, r Runtime, hostFn func(ctx context.Context) uint64, functionName string) ([]byte, func(context.Context) error) {
	mod, err := r.NewModuleBuilder("host").ExportFunction(functionName, hostFn).Instantiate(testCtx)
	require.NoError(t, err)

	return []byte(fmt.Sprintf(
		`(module (import "host" "%[1]s" (func (result i64))) (export "%[1]s" (func 0)))`, functionName,
	)), mod.Close
}

type mockEngine struct {
	name          string
	cachedModules map[*wasm.Module]struct{}
}

// NewModuleEngine implements the same method as documented on wasm.Engine.
func (e *mockEngine) NewModuleEngine(_ string, _ *wasm.Module, _, _ []*wasm.FunctionInstance, _ []*wasm.TableInstance, _ []wasm.TableInitEntry) (wasm.ModuleEngine, error) {
	return nil, nil
}

// DeleteCompiledModule implements the same method as documented on wasm.Engine.
func (e *mockEngine) DeleteCompiledModule(module *wasm.Module) {
	delete(e.cachedModules, module)
}

func (e *mockEngine) CompileModule(_ context.Context, module *wasm.Module) error {
	e.cachedModules[module] = struct{}{}
	return nil
}
