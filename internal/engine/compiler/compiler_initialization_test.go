package compiler

import (
	"fmt"
	"reflect"
	"testing"
	"unsafe"

	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wazeroir"
)

func TestCompiler_compileModuleContextInitialization(t *testing.T) {
	for _, tc := range []struct {
		name           string
		moduleInstance *wasm.ModuleInstance
	}{
		{
			name: "no nil",
			moduleInstance: &wasm.ModuleInstance{
				Globals: []*wasm.GlobalInstance{{Val: 100}},
				Memory:  &wasm.MemoryInstance{Buffer: make([]byte, 10)},
				Tables: []*wasm.TableInstance{
					{References: make([]wasm.Reference, 20)},
					{References: make([]wasm.Reference, 10)},
				},
				TypeIDs:          make([]wasm.FunctionTypeID, 10),
				DataInstances:    make([][]byte, 10),
				ElementInstances: make([]wasm.ElementInstance, 10),
			},
		},
		{
			name: "element instances nil",
			moduleInstance: &wasm.ModuleInstance{
				Globals:          []*wasm.GlobalInstance{{Val: 100}},
				Memory:           &wasm.MemoryInstance{Buffer: make([]byte, 10)},
				Tables:           []*wasm.TableInstance{{References: make([]wasm.Reference, 20)}},
				TypeIDs:          make([]wasm.FunctionTypeID, 10),
				DataInstances:    make([][]byte, 10),
				ElementInstances: nil,
			},
		},
		{
			name: "data instances nil",
			moduleInstance: &wasm.ModuleInstance{
				Globals:          []*wasm.GlobalInstance{{Val: 100}},
				Memory:           &wasm.MemoryInstance{Buffer: make([]byte, 10)},
				Tables:           []*wasm.TableInstance{{References: make([]wasm.Reference, 20)}},
				TypeIDs:          make([]wasm.FunctionTypeID, 10),
				DataInstances:    nil,
				ElementInstances: make([]wasm.ElementInstance, 10),
			},
		},
		{
			name: "globals nil",
			moduleInstance: &wasm.ModuleInstance{
				Memory:           &wasm.MemoryInstance{Buffer: make([]byte, 10)},
				Tables:           []*wasm.TableInstance{{References: make([]wasm.Reference, 20)}},
				TypeIDs:          make([]wasm.FunctionTypeID, 10),
				DataInstances:    make([][]byte, 10),
				ElementInstances: make([]wasm.ElementInstance, 10),
			},
		},
		{
			name: "memory nil",
			moduleInstance: &wasm.ModuleInstance{
				Globals:          []*wasm.GlobalInstance{{Val: 100}},
				Tables:           []*wasm.TableInstance{{References: make([]wasm.Reference, 20)}},
				TypeIDs:          make([]wasm.FunctionTypeID, 10),
				DataInstances:    make([][]byte, 10),
				ElementInstances: make([]wasm.ElementInstance, 10),
			},
		},
		{
			name: "table nil",
			moduleInstance: &wasm.ModuleInstance{
				Memory:           &wasm.MemoryInstance{Buffer: make([]byte, 10)},
				Tables:           []*wasm.TableInstance{{References: nil}},
				TypeIDs:          make([]wasm.FunctionTypeID, 10),
				DataInstances:    make([][]byte, 10),
				ElementInstances: make([]wasm.ElementInstance, 10),
			},
		},
		{
			name: "table empty",
			moduleInstance: &wasm.ModuleInstance{
				Tables:           []*wasm.TableInstance{{References: make([]wasm.Reference, 20)}},
				TypeIDs:          make([]wasm.FunctionTypeID, 10),
				DataInstances:    make([][]byte, 10),
				ElementInstances: make([]wasm.ElementInstance, 10),
			},
		},
		{
			name: "memory zero length",
			moduleInstance: &wasm.ModuleInstance{
				Memory: &wasm.MemoryInstance{Buffer: make([]byte, 0)},
			},
		},
		{
			name:           "all nil except mod engine",
			moduleInstance: &wasm.ModuleInstance{},
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			env := newCompilerEnvironment()
			env.moduleInstance = tc.moduleInstance
			ce := env.callEngine()

			ir := &wazeroir.CompilationResult{
				HasMemory:                     tc.moduleInstance.Memory != nil,
				HasTable:                      len(tc.moduleInstance.Tables) > 0,
				NeedsAccessToDataInstances:    len(tc.moduleInstance.DataInstances) > 0,
				NeedsAccessToElementInstances: len(tc.moduleInstance.ElementInstances) > 0,
			}
			for _, g := range tc.moduleInstance.Globals {
				ir.Globals = append(ir.Globals, g.Type)
			}
			compiler := env.requireNewCompiler(t, newCompiler, ir)
			me := &moduleEngine{functions: make([]*function, 10)}
			tc.moduleInstance.Engine = me

			// The golang-asm assembler skips the first instruction, so we emit NOP here which is ignored.
			// TODO: delete after #233
			compiler.compileNOP()

			err := compiler.compileModuleContextInitialization()
			require.NoError(t, err)
			require.Equal(t, 0, len(compiler.valueLocationStack().usedRegisters), "expected no usedRegisters")

			compiler.compileExitFromNativeCode(compilerCallStatusCodeReturned)

			// Generate the code under test.
			code, _, _, err := compiler.compile()
			require.NoError(t, err)

			env.exec(code)

			// Check the exit status.
			require.Equal(t, compilerCallStatusCodeReturned, env.compilerStatus())

			// Check if the fields of callEngine.moduleContext are updated.
			bufSliceHeader := (*reflect.SliceHeader)(unsafe.Pointer(&tc.moduleInstance.Globals))
			require.Equal(t, bufSliceHeader.Data, ce.moduleContext.globalElement0Address)

			if tc.moduleInstance.Memory != nil {
				bufSliceHeader := (*reflect.SliceHeader)(unsafe.Pointer(&tc.moduleInstance.Memory.Buffer))
				require.Equal(t, uint64(bufSliceHeader.Len), ce.moduleContext.memorySliceLen)
				require.Equal(t, bufSliceHeader.Data, ce.moduleContext.memoryElement0Address)
			}

			if len(tc.moduleInstance.Tables) > 0 {
				tableHeader := (*reflect.SliceHeader)(unsafe.Pointer(&tc.moduleInstance.Tables))
				require.Equal(t, tableHeader.Data, ce.moduleContext.tablesElement0Address)
				require.Equal(t, uintptr(unsafe.Pointer(&tc.moduleInstance.TypeIDs[0])), ce.moduleContext.typeIDsElement0Address)
				require.Equal(t, uintptr(unsafe.Pointer(&tc.moduleInstance.Tables[0])), ce.moduleContext.tablesElement0Address)
			}

			if len(tc.moduleInstance.DataInstances) > 0 {
				dataInstancesHeader := (*reflect.SliceHeader)(unsafe.Pointer(&tc.moduleInstance.DataInstances))
				require.Equal(t, dataInstancesHeader.Data, ce.moduleContext.dataInstancesElement0Address)
				require.Equal(t, uintptr(unsafe.Pointer(&tc.moduleInstance.DataInstances[0])), ce.moduleContext.dataInstancesElement0Address)
			}

			if len(tc.moduleInstance.ElementInstances) > 0 {
				elementInstancesHeader := (*reflect.SliceHeader)(unsafe.Pointer(&tc.moduleInstance.ElementInstances))
				require.Equal(t, elementInstancesHeader.Data, ce.moduleContext.elementInstancesElemen0Address)
				require.Equal(t, uintptr(unsafe.Pointer(&tc.moduleInstance.ElementInstances[0])), ce.moduleContext.elementInstancesElemen0Address)
			}

			require.Equal(t, uintptr(unsafe.Pointer(&me.functions[0])), ce.moduleContext.functionsElement0Address)
		})
	}
}

func TestCompiler_compileMaybeGrowValueStack(t *testing.T) {
	t.Run("not grow", func(t *testing.T) {
		const stackPointerCeil = 5
		for _, baseOffset := range []uint64{5, 10, 20} {
			t.Run(fmt.Sprintf("%d", baseOffset), func(t *testing.T) {
				env := newCompilerEnvironment()
				compiler := env.requireNewCompiler(t, newCompiler, nil)

				// The golang-asm assembler skips the first instruction, so we emit NOP here which is ignored.
				// TODO: delete after #233
				compiler.compileNOP()

				err := compiler.compileMaybeGrowValueStack()
				require.NoError(t, err)
				require.NotNil(t, compiler.getOnStackPointerCeilDeterminedCallBack())

				valueStackLen := uint64(len(env.stack()))
				stackBasePointer := valueStackLen - baseOffset // Ceil <= valueStackLen - stackBasePointer = no need to grow!
				compiler.getOnStackPointerCeilDeterminedCallBack()(stackPointerCeil)
				env.setValueStackBasePointer(stackBasePointer)

				compiler.compileExitFromNativeCode(compilerCallStatusCodeReturned)

				// Generate and run the code under test.
				code, _, _, err := compiler.compile()
				require.NoError(t, err)
				env.exec(code)

				// The status code must be "Returned", not "BuiltinFunctionCall".
				require.Equal(t, compilerCallStatusCodeReturned, env.compilerStatus())
			})
		}
	})
	t.Run("grow", func(t *testing.T) {
		env := newCompilerEnvironment()
		compiler := env.requireNewCompiler(t, newCompiler, nil)

		// The golang-asm assembler skips the first instruction, so we emit NOP here which is ignored.
		// TODO: delete after #233
		compiler.compileNOP()

		err := compiler.compileMaybeGrowValueStack()
		require.NoError(t, err)

		// On the return from grow value stack, we simply return.
		err = compiler.compileReturnFunction()
		require.NoError(t, err)

		stackPointerCeil := uint64(6)
		compiler.setStackPointerCeil(stackPointerCeil)
		valueStackLen := uint64(len(env.stack()))
		stackBasePointer := valueStackLen - 5 // Ceil > valueStackLen - stackBasePointer = need to grow!
		env.setValueStackBasePointer(stackBasePointer)

		// Generate and run the code under test.
		code, _, _, err := compiler.compile()
		require.NoError(t, err)
		env.exec(code)

		// Check if the call exits with builtin function call status.
		require.Equal(t, compilerCallStatusCodeCallBuiltInFunction, env.compilerStatus())

		// Reenter from the return address.
		returnAddress := env.callFrameStackPeek().returnAddress
		require.True(t, returnAddress != 0, "returnAddress was non-zero %d", returnAddress)
		compilercall(
			returnAddress, uintptr(unsafe.Pointer(env.callEngine())),
			uintptr(unsafe.Pointer(env.module())),
		)

		// Check the result. This should be "Returned".
		require.Equal(t, compilerCallStatusCodeReturned, env.compilerStatus())
	})
}
