package compiler

import (
	"math"
	"os"
	"runtime"
	"testing"
	"unsafe"

	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
	"github.com/tetratelabs/wazero/internal/wazeroir"
)

func TestMain(m *testing.M) {
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		// Compiler is currently implemented only for amd64 or arm64.
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type compilerEnv struct {
	me             *moduleEngine
	ce             *callEngine
	moduleInstance *wasm.ModuleInstance
}

func (j *compilerEnv) stackTopAsUint32() uint32 {
	return uint32(j.stack()[j.stackPointer()-1])
}

func (j *compilerEnv) stackTopAsInt32() int32 {
	return int32(j.stack()[j.stackPointer()-1])
}
func (j *compilerEnv) stackTopAsUint64() uint64 {
	return j.stack()[j.stackPointer()-1]
}

func (j *compilerEnv) stackTopAsInt64() int64 {
	return int64(j.stack()[j.stackPointer()-1])
}

func (j *compilerEnv) stackTopAsFloat32() float32 {
	return math.Float32frombits(uint32(j.stack()[j.stackPointer()-1]))
}

func (j *compilerEnv) stackTopAsFloat64() float64 {
	return math.Float64frombits(j.stack()[j.stackPointer()-1])
}

func (j *compilerEnv) memory() []byte {
	return j.moduleInstance.Memory.Buffer
}

func (j *compilerEnv) stack() []uint64 {
	return j.ce.valueStack
}

func (j *compilerEnv) compilerStatus() compilerCallStatusCode {
	return j.ce.exitContext.statusCode
}

func (j *compilerEnv) builtinFunctionCallAddress() wasm.Index {
	return j.ce.exitContext.builtinFunctionCallIndex
}

func (j *compilerEnv) stackPointer() uint64 {
	return j.ce.valueStackContext.stackPointer
}

func (j *compilerEnv) stackBasePointer() uint64 {
	return j.ce.valueStackContext.stackBasePointer
}

func (j *compilerEnv) setStackPointer(sp uint64) {
	j.ce.valueStackContext.stackPointer = sp
}

func (j *compilerEnv) addGlobals(g ...*wasm.GlobalInstance) {
	j.moduleInstance.Globals = append(j.moduleInstance.Globals, g...)
}

func (j *compilerEnv) getGlobal(index uint32) uint64 {
	return j.moduleInstance.Globals[index].Val
}

func (j *compilerEnv) addTable(table *wasm.TableInstance) {
	j.moduleInstance.Tables = append(j.moduleInstance.Tables, table)
}

func (j *compilerEnv) callFrameStackPeek() *callFrame {
	return &j.ce.callFrameStack[j.ce.globalContext.callFrameStackPointer-1]
}

func (j *compilerEnv) callFrameStackPointer() uint64 {
	return j.ce.globalContext.callFrameStackPointer
}

func (j *compilerEnv) setValueStackBasePointer(sp uint64) {
	j.ce.valueStackContext.stackBasePointer = sp
}

func (j *compilerEnv) setCallFrameStackPointerLen(l uint64) {
	j.ce.callFrameStackLen = l
}

func (j *compilerEnv) module() *wasm.ModuleInstance {
	return j.moduleInstance
}

func (j *compilerEnv) moduleEngine() *moduleEngine {
	return j.me
}

func (j *compilerEnv) callEngine() *callEngine {
	return j.ce
}

func (j *compilerEnv) exec(codeSegment []byte) {
	f := &function{
		parent:                &code{codeSegment: codeSegment},
		codeInitialAddress:    uintptr(unsafe.Pointer(&codeSegment[0])),
		moduleInstanceAddress: uintptr(unsafe.Pointer(j.moduleInstance)),
		source: &wasm.FunctionInstance{
			Kind:   wasm.FunctionKindWasm,
			Type:   &wasm.FunctionType{},
			Module: j.moduleInstance,
		},
	}

	j.ce.callFrameStack[j.ce.globalContext.callFrameStackPointer] = callFrame{function: f}
	j.ce.globalContext.callFrameStackPointer++

	compilercall(
		uintptr(unsafe.Pointer(&codeSegment[0])),
		uintptr(unsafe.Pointer(j.ce)),
		uintptr(unsafe.Pointer(j.moduleInstance)),
	)
}

// newTestCompiler allows us to test a different architecture than the current one.
type newTestCompiler func(ir *wazeroir.CompilationResult) (compiler, error)

func (j *compilerEnv) requireNewCompiler(t *testing.T, fn newTestCompiler, ir *wazeroir.CompilationResult) compilerImpl {
	requireSupportedOSArch(t)

	if ir == nil {
		ir = &wazeroir.CompilationResult{
			LabelCallers: map[string]uint32{},
			Signature:    &wasm.FunctionType{},
		}
	}
	c, err := fn(ir)

	require.NoError(t, err)

	ret, ok := c.(compilerImpl)
	require.True(t, ok)
	return ret
}

// CompilerImpl is the interface used for architecture-independent unit tests in this file.
// This is currently implemented by amd64 and arm64.
type compilerImpl interface {
	compiler
	compileExitFromNativeCode(compilerCallStatusCode)
	compileMaybeGrowValueStack() error
	compileReturnFunction() error
	getOnStackPointerCeilDeterminedCallBack() func(uint64)
	setStackPointerCeil(uint64)
	compileReleaseRegisterToStack(loc *valueLocation)
	valueLocationStack() *valueLocationStack
	setValueLocationStack(*valueLocationStack)
	compileEnsureOnGeneralPurposeRegister(loc *valueLocation) error
	compileModuleContextInitialization() error
	compileNOP()
}

const defaultMemoryPageNumInTest = 1

func newCompilerEnvironment() *compilerEnv {
	me := &moduleEngine{}
	return &compilerEnv{
		me: me,
		moduleInstance: &wasm.ModuleInstance{
			Memory:  &wasm.MemoryInstance{Buffer: make([]byte, wasm.MemoryPageSize*defaultMemoryPageNumInTest)},
			Tables:  []*wasm.TableInstance{},
			Globals: []*wasm.GlobalInstance{},
			Engine:  me,
		},
		ce: me.newCallEngine(),
	}
}
