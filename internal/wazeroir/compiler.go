package wazeroir

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strings"

	"github.com/tetratelabs/wazero/internal/buildoptions"
	"github.com/tetratelabs/wazero/internal/leb128"
	"github.com/tetratelabs/wazero/internal/wasm"
)

type controlFrameKind byte

const (
	controlFrameKindBlockWithContinuationLabel controlFrameKind = iota
	controlFrameKindBlockWithoutContinuationLabel
	controlFrameKindFunction
	controlFrameKindLoop
	controlFrameKindIfWithElse
	controlFrameKindIfWithoutElse
)

type (
	controlFrame struct {
		frameID uint32
		// originalStackLen holds the number of values on the stack
		// when start executing this control frame minus params for the block.
		originalStackLenWithoutParam int
		blockType                    *wasm.FunctionType
		kind                         controlFrameKind
	}
	controlFrames struct{ frames []*controlFrame }
)

func (c *controlFrame) ensureContinuation() {
	// Make sure that if the frame is block and doesn't have continuation,
	// change the kind so we can emit the continuation block
	// later when we reach the end instruction of this frame.
	if c.kind == controlFrameKindBlockWithoutContinuationLabel {
		c.kind = controlFrameKindBlockWithContinuationLabel
	}
}

func (c *controlFrame) asBranchTarget() *BranchTarget {
	switch c.kind {
	case controlFrameKindBlockWithContinuationLabel,
		controlFrameKindBlockWithoutContinuationLabel:
		return &BranchTarget{Label: &Label{FrameID: c.frameID, Kind: LabelKindContinuation}}
	case controlFrameKindLoop:
		return &BranchTarget{Label: &Label{FrameID: c.frameID, Kind: LabelKindHeader}}
	case controlFrameKindFunction:
		// Note nil target is translated as return.
		return &BranchTarget{Label: nil}
	case controlFrameKindIfWithElse,
		controlFrameKindIfWithoutElse:
		return &BranchTarget{Label: &Label{FrameID: c.frameID, Kind: LabelKindContinuation}}
	}
	panic(fmt.Sprintf("unreachable: a bug in wazeroir implementation: %v", c.kind))
}

func (c *controlFrames) functionFrame() *controlFrame {
	// No need to check stack bound
	// as we can assume that all the operations
	// are valid thanks to validateFunction
	// at module validation phase.
	return c.frames[0]
}

func (c *controlFrames) get(n int) *controlFrame {
	// No need to check stack bound
	// as we can assume that all the operations
	// are valid thanks to validateFunction
	// at module validation phase.
	return c.frames[len(c.frames)-n-1]
}

func (c *controlFrames) top() *controlFrame {
	// No need to check stack bound
	// as we can assume that all the operations
	// are valid thanks to validateFunction
	// at module validation phase.
	return c.frames[len(c.frames)-1]
}

func (c *controlFrames) empty() bool {
	return len(c.frames) == 0
}

func (c *controlFrames) pop() (frame *controlFrame) {
	// No need to check stack bound
	// as we can assume that all the operations
	// are valid thanks to validateFunction
	// at module validation phase.
	frame = c.top()
	c.frames = c.frames[:len(c.frames)-1]
	return
}

func (c *controlFrames) push(frame *controlFrame) {
	c.frames = append(c.frames, frame)
}

// localIndexToStackHeight initializes localIndexToStackHeight field. See the comment on localIndexToStackHeight.
func (c *compiler) calcLocalIndexToStackHeight() {
	c.localIndexToStackHeight = make(map[uint32]int, len(c.sig.Params)+len(c.localTypes))
	var current int
	for index, lt := range c.sig.Params {
		c.localIndexToStackHeight[wasm.Index(index)] = current
		if lt == wasm.ValueTypeV128 {
			current++
		}
		current++
	}

	for index, lt := range c.localTypes {
		index += len(c.sig.Params)
		c.localIndexToStackHeight[wasm.Index(index)] = current
		if lt == wasm.ValueTypeV128 {
			current++
		}
		current++
	}
}

type compiler struct {
	enabledFeatures  wasm.Features
	stack            []UnsignedType
	currentID        uint32
	controlFrames    *controlFrames
	unreachableState struct {
		on    bool
		depth int
	}
	pc     uint64
	result CompilationResult

	// body holds the code for the function's body where Wasm instructions are stored.
	body []byte
	// sig is the function type of the target function.
	sig *wasm.FunctionType
	// localTypes holds the target function locals' value types except function params.
	localTypes []wasm.ValueType
	// localIndexToStackHeight maps the local index (starting with function params) to the stack height
	// where the local is places. This is the necessary mapping for functions who contain vector type locals.
	localIndexToStackHeight map[wasm.Index]int

	// types hold all the function types in the module where the targe function exists.
	types []*wasm.FunctionType
	// funcs holds the type indexes for all declard functions in the module where the targe function exists.
	funcs []uint32
	// globals holds the global types for all declard globas in the module where the targe function exists.
	globals []*wasm.GlobalType
}

// For debugging only.
//nolint
func (c *compiler) stackDump() string {
	strs := make([]string, 0, len(c.stack))
	for _, s := range c.stack {
		strs = append(strs, s.String())
	}
	return "[" + strings.Join(strs, ", ") + "]"
}

func (c *compiler) markUnreachable() {
	c.unreachableState.on = true
}

func (c *compiler) resetUnreachable() {
	c.unreachableState.on = false
}

type CompilationResult struct {
	// Operations holds wazeroir operations compiled from Wasm instructions in a Wasm function.
	Operations []Operation
	// LabelCallers maps Label.String() to the number of callers to that label.
	// Here "callers" means that the call-sites which jumps to the label with br, br_if or br_table
	// instructions.
	//
	// Note: zero possible and allowed in wasm. Ex.
	//
	//	(block
	//	  (br 0)
	//	  (block i32.const 1111)
	//	)
	//
	// This example the label corresponding to `(block i32.const 1111)` is never be reached at runtime because `br 0` exits the function before we reach there
	LabelCallers map[string]uint32

	// Signature is the function type of the compilation target function.
	Signature *wasm.FunctionType
	// Globals holds all the declarations of globals in the module from which this function is compiled.
	Globals []*wasm.GlobalType
	// Functions holds all the declarations of function in the module from which this function is compiled, including itself.
	Functions []wasm.Index
	// Types holds all the types in the module from which this function is compiled.
	Types []*wasm.FunctionType
	// TableTypes holds all the reference types of all tables declared in the module.
	TableTypes []wasm.ValueType
	// HasMemory is true if the module from which this function is compiled has memory declaration.
	HasMemory bool
	// HasTable is true if the module from which this function is compiled has table declaration.
	HasTable bool
	// NeedsAccessToDataInstances is true if the function needs access to data instances via memory.init or data.drop instructions.
	NeedsAccessToDataInstances bool
	// NeedsAccessToDataInstances is true if the function needs access to element instances via table.init or elem.drop instructions.
	NeedsAccessToElementInstances bool
}

func CompileFunctions(_ context.Context, enabledFeatures wasm.Features, module *wasm.Module) ([]*CompilationResult, error) {
	// Note: If you use the context.Context param, don't forget to coerce nil to context.Background()!

	functions, globals, mem, tables, err := module.AllDeclarations()
	if err != nil {
		return nil, err
	}

	hasMemory, hasTable := mem != nil, len(tables) > 0

	tableTypes := make([]wasm.ValueType, len(tables))
	for i := range tableTypes {
		tableTypes[i] = tables[i].Type
	}

	var ret []*CompilationResult
	for funcInxdex := range module.FunctionSection {
		typeID := module.FunctionSection[funcInxdex]
		sig := module.TypeSection[typeID]
		code := module.CodeSection[funcInxdex]
		r, err := compile(enabledFeatures, sig, code.Body, code.LocalTypes, module.TypeSection, functions, globals)
		if err != nil {
			return nil, fmt.Errorf("failed to lower func[%d/%d] to wazeroir: %w", funcInxdex, len(functions)-1, err)
		}
		r.Globals = globals
		r.Functions = functions
		r.Types = module.TypeSection
		r.HasMemory = hasMemory
		r.HasTable = hasTable
		r.Signature = sig
		r.TableTypes = tableTypes
		ret = append(ret, r)
	}
	return ret, nil
}

// Compile lowers given function instance into wazeroir operations
// so that the resulting operations can be consumed by the interpreter
// or the Compiler compilation engine.
func compile(enabledFeatures wasm.Features,
	sig *wasm.FunctionType,
	body []byte,
	localTypes []wasm.ValueType,
	types []*wasm.FunctionType,
	functions []uint32, globals []*wasm.GlobalType,
) (*CompilationResult, error) {
	c := compiler{
		enabledFeatures: enabledFeatures,
		controlFrames:   &controlFrames{},
		result:          CompilationResult{LabelCallers: map[string]uint32{}},
		body:            body,
		localTypes:      localTypes,
		sig:             sig,
		globals:         globals,
		funcs:           functions,
		types:           types,
	}

	c.calcLocalIndexToStackHeight()

	// Push function arguments.
	for _, t := range sig.Params {
		c.stackPush(wasmValueTypeToUnsignedType(t)...)
	}
	// Emit const expressions for locals.
	// Note that here we don't take function arguments
	// into account, meaning that callers must push
	// arguments before entering into the function body.
	for _, t := range localTypes {
		c.emitDefaultValue(t)
	}

	// Insert the function control frame.
	c.controlFrames.push(&controlFrame{
		frameID:   c.nextID(),
		blockType: c.sig,
		kind:      controlFrameKindFunction,
	})

	// Now, enter the function body.
	for !c.controlFrames.empty() && c.pc < uint64(len(c.body)) {
		if err := c.handleInstruction(); err != nil {
			return nil, fmt.Errorf("handling instruction: %w", err)
		}
	}
	return &c.result, nil
}

// Translate the current Wasm instruction to wazeroir's operations,
// and emit the results into c.results.
func (c *compiler) handleInstruction() error {
	op := c.body[c.pc]
	if buildoptions.IsDebugMode {
		fmt.Printf("handling %s, unreachable_state(on=%v,depth=%d)\n",
			wasm.InstructionName(op),
			c.unreachableState.on, c.unreachableState.depth,
		)
	}

	// Modify the stack according the current instruction.
	// Note that some instructions will read "index" in
	// applyToStack and advance c.pc inside the function.
	index, err := c.applyToStack(op)
	if err != nil {
		return fmt.Errorf("apply stack failed for %s: %w", wasm.InstructionName(op), err)
	}
	// Now we handle each instruction, and
	// emit the corresponding wazeroir operations to the results.
operatorSwitch:
	switch op {
	case wasm.OpcodeUnreachable:
		c.emit(
			&OperationUnreachable{},
		)
		c.markUnreachable()
	case wasm.OpcodeNop:
		// Nop is noop!
	case wasm.OpcodeBlock:
		bt, num, err := wasm.DecodeBlockType(c.types,
			bytes.NewReader(c.body[c.pc+1:]), c.enabledFeatures)
		if err != nil {
			return fmt.Errorf("reading block type for block instruction: %w", err)
		}
		c.pc += num

		if c.unreachableState.on {
			// If it is currently in unreachable,
			// just remove the entire block.
			c.unreachableState.depth++
			break operatorSwitch
		}

		// Create a new frame -- entering this block.
		frame := &controlFrame{
			frameID:                      c.nextID(),
			originalStackLenWithoutParam: len(c.stack) - bt.ParamNumInUint64,
			kind:                         controlFrameKindBlockWithoutContinuationLabel,
			blockType:                    bt,
		}
		c.controlFrames.push(frame)

	case wasm.OpcodeLoop:
		bt, num, err := wasm.DecodeBlockType(c.types, bytes.NewReader(c.body[c.pc+1:]), c.enabledFeatures)
		if err != nil {
			return fmt.Errorf("reading block type for loop instruction: %w", err)
		}
		c.pc += num

		if c.unreachableState.on {
			// If it is currently in unreachable,
			// just remove the entire block.
			c.unreachableState.depth++
			break operatorSwitch
		}

		// Create a new frame -- entering loop.
		frame := &controlFrame{
			frameID:                      c.nextID(),
			originalStackLenWithoutParam: len(c.stack) - bt.ParamNumInUint64,
			kind:                         controlFrameKindLoop,
			blockType:                    bt,
		}
		c.controlFrames.push(frame)

		// Prep labels for inside and the continuation of this loop.
		loopLabel := &Label{FrameID: frame.frameID, Kind: LabelKindHeader}
		c.result.LabelCallers[loopLabel.String()]++

		// Emit the branch operation to enter inside the loop.
		c.emit(
			&OperationBr{
				Target: loopLabel.asBranchTarget(),
			},
			&OperationLabel{Label: loopLabel},
		)

	case wasm.OpcodeIf:
		bt, num, err := wasm.DecodeBlockType(c.types, bytes.NewReader(c.body[c.pc+1:]), c.enabledFeatures)
		if err != nil {
			return fmt.Errorf("reading block type for if instruction: %w", err)
		}
		c.pc += num

		if c.unreachableState.on {
			// If it is currently in unreachable,
			// just remove the entire block.
			c.unreachableState.depth++
			break operatorSwitch
		}

		// Create a new frame -- entering if.
		frame := &controlFrame{
			frameID:                      c.nextID(),
			originalStackLenWithoutParam: len(c.stack) - bt.ParamNumInUint64,
			// Note this will be set to controlFrameKindIfWithElse
			// when else opcode found later.
			kind:      controlFrameKindIfWithoutElse,
			blockType: bt,
		}
		c.controlFrames.push(frame)

		// Prep labels for if and else of this if.
		thenLabel := &Label{Kind: LabelKindHeader, FrameID: frame.frameID}
		elseLabel := &Label{Kind: LabelKindElse, FrameID: frame.frameID}
		c.result.LabelCallers[thenLabel.String()]++
		c.result.LabelCallers[elseLabel.String()]++

		// Emit the branch operation to enter the then block.
		c.emit(
			&OperationBrIf{
				Then: thenLabel.asBranchTargetDrop(),
				Else: elseLabel.asBranchTargetDrop(),
			},
			&OperationLabel{
				Label: thenLabel,
			},
		)
	case wasm.OpcodeElse:
		frame := c.controlFrames.top()
		if c.unreachableState.on && c.unreachableState.depth > 0 {
			// If it is currently in unreachable, and the nested if,
			// just remove the entire else block.
			break operatorSwitch
		} else if c.unreachableState.on {
			// If it is currently in unreachable, and the non-nested if,
			// reset the stack so we can correctly handle the else block.
			top := c.controlFrames.top()
			c.stack = c.stack[:top.originalStackLenWithoutParam]
			top.kind = controlFrameKindIfWithElse

			// Re-push the parameters to the if block so that else block can use them.
			for _, t := range frame.blockType.Params {
				c.stackPush(wasmValueTypeToUnsignedType(t)...)
			}

			// We are no longer unreachable in else frame,
			// so emit the correct label, and reset the unreachable state.
			elseLabel := &Label{FrameID: frame.frameID, Kind: LabelKindElse}
			c.resetUnreachable()
			c.emit(
				&OperationLabel{Label: elseLabel},
			)
			break operatorSwitch
		}

		// Change the kind of this If block, indicating that
		// the if has else block.
		frame.kind = controlFrameKindIfWithElse

		// We need to reset the stack so that
		// the values pushed inside the then block
		// do not affect the else block.
		dropOp := &OperationDrop{Depth: c.getFrameDropRange(frame, false)}

		// Reset the stack manipulated by the then block, and re-push the block param types to the stack.

		c.stack = c.stack[:frame.originalStackLenWithoutParam]
		for _, t := range frame.blockType.Params {
			c.stackPush(wasmValueTypeToUnsignedType(t)...)
		}

		// Prep labels for else and the continuation of this if block.
		elseLabel := &Label{FrameID: frame.frameID, Kind: LabelKindElse}
		continuationLabel := &Label{FrameID: frame.frameID, Kind: LabelKindContinuation}
		c.result.LabelCallers[continuationLabel.String()]++

		// Emit the instructions for exiting the if loop,
		// and then the initiation of else block.
		c.emit(
			dropOp,
			// Jump to the continuation of this block.
			&OperationBr{Target: continuationLabel.asBranchTarget()},
			// Initiate the else block.
			&OperationLabel{Label: elseLabel},
		)
	case wasm.OpcodeEnd:
		if c.unreachableState.on && c.unreachableState.depth > 0 {
			c.unreachableState.depth--
			break operatorSwitch
		} else if c.unreachableState.on {
			c.resetUnreachable()

			frame := c.controlFrames.pop()
			if c.controlFrames.empty() {
				return nil
			}

			c.stack = c.stack[:frame.originalStackLenWithoutParam]
			for _, t := range frame.blockType.Results {
				c.stackPush(wasmValueTypeToUnsignedType(t)...)
			}

			continuationLabel := &Label{FrameID: frame.frameID, Kind: LabelKindContinuation}
			if frame.kind == controlFrameKindIfWithoutElse {
				// Emit the else label.
				elseLabel := &Label{Kind: LabelKindElse, FrameID: frame.frameID}
				c.result.LabelCallers[continuationLabel.String()]++
				c.emit(
					&OperationLabel{Label: elseLabel},
					&OperationBr{Target: continuationLabel.asBranchTarget()},
					&OperationLabel{Label: continuationLabel},
				)
			} else {
				c.emit(
					&OperationLabel{Label: continuationLabel},
				)
			}

			break operatorSwitch
		}

		frame := c.controlFrames.pop()

		// We need to reset the stack so that
		// the values pushed inside the block.
		dropOp := &OperationDrop{Depth: c.getFrameDropRange(frame, true)}
		c.stack = c.stack[:frame.originalStackLenWithoutParam]

		// Push the result types onto the stack.
		for _, t := range frame.blockType.Results {
			c.stackPush(wasmValueTypeToUnsignedType(t)...)
		}

		// Emit the instructions according to the kind of the current control frame.
		switch frame.kind {
		case controlFrameKindFunction:
			if !c.controlFrames.empty() {
				// Should never happen. If so, there's a bug in the translation.
				panic("bug: found more function control frames")
			}
			// Return from function.
			c.emit(
				dropOp,
				// Pass empty target instead of nil to avoid misinterpretation as "return"
				&OperationBr{Target: &BranchTarget{}},
			)
		case controlFrameKindIfWithoutElse:
			// This case we have to emit "empty" else label.
			elseLabel := &Label{Kind: LabelKindElse, FrameID: frame.frameID}
			continuationLabel := &Label{Kind: LabelKindContinuation, FrameID: frame.frameID}
			c.result.LabelCallers[continuationLabel.String()] += 2
			c.emit(
				dropOp,
				&OperationBr{Target: continuationLabel.asBranchTarget()},
				// Emit the else which soon branches into the continuation.
				&OperationLabel{Label: elseLabel},
				&OperationBr{Target: continuationLabel.asBranchTarget()},
				// Initiate the continuation.
				&OperationLabel{Label: continuationLabel},
			)
		case controlFrameKindBlockWithContinuationLabel,
			controlFrameKindIfWithElse:
			continuationLabel := &Label{Kind: LabelKindContinuation, FrameID: frame.frameID}
			c.result.LabelCallers[continuationLabel.String()]++
			c.emit(
				dropOp,
				&OperationBr{Target: continuationLabel.asBranchTarget()},
				&OperationLabel{Label: continuationLabel},
			)
		case controlFrameKindLoop, controlFrameKindBlockWithoutContinuationLabel:
			c.emit(
				dropOp,
			)
		default:
			// Should never happen. If so, there's a bug in the translation.
			panic(fmt.Errorf("bug: invalid control frame kind: 0x%x", frame.kind))
		}

	case wasm.OpcodeBr:
		targetIndex, n, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
		if err != nil {
			return fmt.Errorf("read the target for br_if: %w", err)
		}
		c.pc += n

		targetFrame := c.controlFrames.get(int(targetIndex))
		targetFrame.ensureContinuation()
		dropOp := &OperationDrop{Depth: c.getFrameDropRange(targetFrame, false)}
		target := targetFrame.asBranchTarget()
		c.result.LabelCallers[target.Label.String()]++
		c.emit(
			dropOp,
			&OperationBr{Target: target},
		)
		// Br operation is stack-polymorphic, and mark the state as unreachable.
		// That means subsequent instructions in the current control frame are "unreachable"
		// and can be safely removed.
		c.markUnreachable()
	case wasm.OpcodeBrIf:
		targetIndex, n, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
		if err != nil {
			return fmt.Errorf("read the target for br_if: %w", err)
		}
		c.pc += n

		targetFrame := c.controlFrames.get(int(targetIndex))
		targetFrame.ensureContinuation()
		drop := c.getFrameDropRange(targetFrame, false)
		target := targetFrame.asBranchTarget()
		c.result.LabelCallers[target.Label.String()]++

		continuationLabel := &Label{FrameID: c.nextID(), Kind: LabelKindHeader}
		c.result.LabelCallers[continuationLabel.String()]++
		c.emit(
			&OperationBrIf{
				Then: &BranchTargetDrop{ToDrop: drop, Target: target},
				Else: continuationLabel.asBranchTargetDrop(),
			},
			// Start emitting else block operations.
			&OperationLabel{
				Label: continuationLabel,
			},
		)
	case wasm.OpcodeBrTable:
		r := bytes.NewReader(c.body[c.pc+1:])
		numTargets, n, err := leb128.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("error reading number of targets in br_table: %w", err)
		}
		c.pc += n

		// Read the branch targets.
		targets := make([]*BranchTargetDrop, numTargets)
		for i := range targets {
			l, n, err := leb128.DecodeUint32(r)
			if err != nil {
				return fmt.Errorf("error reading target %d in br_table: %w", i, err)
			}
			c.pc += n
			targetFrame := c.controlFrames.get(int(l))
			targetFrame.ensureContinuation()
			drop := c.getFrameDropRange(targetFrame, false)
			target := &BranchTargetDrop{ToDrop: drop, Target: targetFrame.asBranchTarget()}
			targets[i] = target
			c.result.LabelCallers[target.Target.Label.String()]++
		}

		// Prep default target control frame.
		l, n, err := leb128.DecodeUint32(r)
		if err != nil {
			return fmt.Errorf("error reading default target of br_table: %w", err)
		}
		c.pc += n
		defaultTargetFrame := c.controlFrames.get(int(l))
		defaultTargetFrame.ensureContinuation()
		defaultTargetDrop := c.getFrameDropRange(defaultTargetFrame, false)
		defaultTarget := defaultTargetFrame.asBranchTarget()
		c.result.LabelCallers[defaultTarget.Label.String()]++

		c.emit(
			&OperationBrTable{
				Targets: targets,
				Default: &BranchTargetDrop{
					ToDrop: defaultTargetDrop, Target: defaultTarget,
				},
			},
		)
		// Br operation is stack-polymorphic, and mark the state as unreachable.
		// That means subsequent instructions in the current control frame are "unreachable"
		// and can be safely removed.
		c.markUnreachable()
	case wasm.OpcodeReturn:
		functionFrame := c.controlFrames.functionFrame()
		dropOp := &OperationDrop{Depth: c.getFrameDropRange(functionFrame, false)}

		// Cleanup the stack and then jmp to function frame's continuation (meaning return).
		c.emit(
			dropOp,
			&OperationBr{Target: functionFrame.asBranchTarget()},
		)

		// Return operation is stack-polymorphic, and mark the state as unreachable.
		// That means subsequent instructions in the current control frame are "unreachable"
		// and can be safely removed.
		c.markUnreachable()
	case wasm.OpcodeCall:
		if index == nil {
			return fmt.Errorf("index does not exist for function call")
		}
		c.emit(
			&OperationCall{FunctionIndex: *index},
		)
	case wasm.OpcodeCallIndirect:
		if index == nil {
			return fmt.Errorf("index does not exist for indirect function call")
		}
		tableIndex, n, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
		if err != nil {
			return fmt.Errorf("read target for br_table: %w", err)
		}
		c.pc += n
		c.emit(
			&OperationCallIndirect{TypeIndex: *index, TableIndex: tableIndex},
		)
	case wasm.OpcodeDrop:
		c.emit(
			&OperationDrop{Depth: &InclusiveRange{Start: 0, End: 0}},
		)
	case wasm.OpcodeSelect:
		c.emit(
			&OperationSelect{},
		)
	case wasm.OpcodeTypedSelect:
		// Skips two bytes: vector size fixed to 1, and the value type for select.
		c.pc += 2
		// Typed select is semantically equivalent to select at runtime.
		c.emit(
			&OperationSelect{},
		)
	case wasm.OpcodeLocalGet:
		if index == nil {
			return fmt.Errorf("index does not exist for local.get")
		}
		id := *index
		depth := c.localDepth(id)
		if c.localType(id) == wasm.ValueTypeV128 {
			c.emit(
				// -2 because we already pushed the result of this operation into the c.stack before
				// called localDepth ^^,
				&OperationPick{Depth: depth - 2},
				&OperationPick{Depth: depth - 2},
			)
		} else {
			c.emit(
				// -1 because we already manipulated the stack before
				// called localDepth ^^.
				&OperationPick{Depth: depth - 1},
			)
		}
	case wasm.OpcodeLocalSet:
		if index == nil {
			return fmt.Errorf("index does not exist for local.set")
		}
		id := *index
		depth := c.localDepth(id)
		if c.localType(id) == wasm.ValueTypeV128 {
			c.emit(
				// +1 because we already popped the operands for this operation from the c.stack before
				// called localDepth ^^,
				&OperationSwap{Depth: depth + 1},
				&OperationDrop{Depth: &InclusiveRange{Start: 0, End: 0}},
				&OperationSwap{Depth: depth + 1},
				&OperationDrop{Depth: &InclusiveRange{Start: 0, End: 0}},
			)
		} else {
			c.emit(
				// +1 because we already popped the operands for this operation from the c.stack before
				// called localDepth ^^,
				&OperationSwap{Depth: depth + 1},
				&OperationDrop{Depth: &InclusiveRange{Start: 0, End: 0}},
			)
		}
	case wasm.OpcodeLocalTee:
		if index == nil {
			return fmt.Errorf("index does not exist for local.tee")
		}
		id := *index
		depth := c.localDepth(id)
		if c.localType(id) == wasm.ValueTypeV128 {
			c.emit(
				&OperationPick{Depth: 1},
				&OperationPick{Depth: 1},
				&OperationSwap{Depth: depth + 1},
				&OperationDrop{Depth: &InclusiveRange{Start: 0, End: 0}},
				&OperationSwap{Depth: depth + 1},
				&OperationDrop{Depth: &InclusiveRange{Start: 0, End: 0}},
			)
		} else {
			c.emit(
				&OperationPick{Depth: 0},
				&OperationSwap{Depth: depth + 1},
				&OperationDrop{Depth: &InclusiveRange{Start: 0, End: 0}},
			)
		}
	case wasm.OpcodeGlobalGet:
		if index == nil {
			return fmt.Errorf("index does not exist for global.get")
		}
		c.emit(
			&OperationGlobalGet{Index: *index},
		)
	case wasm.OpcodeGlobalSet:
		if index == nil {
			return fmt.Errorf("index does not exist for global.set")
		}
		c.emit(
			&OperationGlobalSet{Index: *index},
		)
	case wasm.OpcodeI32Load:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI32LoadName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad{Type: UnsignedTypeI32, Arg: imm},
		)
	case wasm.OpcodeI64Load:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64LoadName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad{Type: UnsignedTypeI64, Arg: imm},
		)
	case wasm.OpcodeF32Load:
		imm, err := c.readMemoryImmediate(wasm.OpcodeF32LoadName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad{Type: UnsignedTypeF32, Arg: imm},
		)
	case wasm.OpcodeF64Load:
		imm, err := c.readMemoryImmediate(wasm.OpcodeF64LoadName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad{Type: UnsignedTypeF64, Arg: imm},
		)
	case wasm.OpcodeI32Load8S:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI32Load8SName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad8{Type: SignedInt32, Arg: imm},
		)
	case wasm.OpcodeI32Load8U:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI32Load8UName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad8{Type: SignedUint32, Arg: imm},
		)
	case wasm.OpcodeI32Load16S:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI32Load16SName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad16{Type: SignedInt32, Arg: imm},
		)
	case wasm.OpcodeI32Load16U:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI32Load16UName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad16{Type: SignedUint32, Arg: imm},
		)
	case wasm.OpcodeI64Load8S:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64Load8SName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad8{Type: SignedInt64, Arg: imm},
		)
	case wasm.OpcodeI64Load8U:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64Load8UName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad8{Type: SignedUint64, Arg: imm},
		)
	case wasm.OpcodeI64Load16S:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64Load16SName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad16{Type: SignedInt64, Arg: imm},
		)
	case wasm.OpcodeI64Load16U:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64Load16UName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad16{Type: SignedUint64, Arg: imm},
		)
	case wasm.OpcodeI64Load32S:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64Load32SName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad32{Signed: true, Arg: imm},
		)
	case wasm.OpcodeI64Load32U:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64Load32UName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationLoad32{Signed: false, Arg: imm},
		)
	case wasm.OpcodeI32Store:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI32StoreName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationStore{Type: UnsignedTypeI32, Arg: imm},
		)
	case wasm.OpcodeI64Store:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64StoreName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationStore{Type: UnsignedTypeI64, Arg: imm},
		)
	case wasm.OpcodeF32Store:
		imm, err := c.readMemoryImmediate(wasm.OpcodeF32StoreName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationStore{Type: UnsignedTypeF32, Arg: imm},
		)
	case wasm.OpcodeF64Store:
		imm, err := c.readMemoryImmediate(wasm.OpcodeF64StoreName)
		if err != nil {
			return err
		}
		c.emit(
			&OperationStore{Type: UnsignedTypeF64, Arg: imm},
		)
	case wasm.OpcodeI32Store8:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI32Store8Name)
		if err != nil {
			return err
		}
		c.emit(
			&OperationStore8{Type: UnsignedInt32, Arg: imm},
		)
	case wasm.OpcodeI32Store16:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI32Store16Name)
		if err != nil {
			return err
		}
		c.emit(
			&OperationStore16{Type: UnsignedInt32, Arg: imm},
		)
	case wasm.OpcodeI64Store8:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64Store8Name)
		if err != nil {
			return err
		}
		c.emit(
			&OperationStore8{Type: UnsignedInt64, Arg: imm},
		)
	case wasm.OpcodeI64Store16:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64Store16Name)
		if err != nil {
			return err
		}
		c.emit(
			&OperationStore16{Type: UnsignedInt64, Arg: imm},
		)
	case wasm.OpcodeI64Store32:
		imm, err := c.readMemoryImmediate(wasm.OpcodeI64Store32Name)
		if err != nil {
			return err
		}
		c.emit(
			&OperationStore32{Arg: imm},
		)
	case wasm.OpcodeMemorySize:
		c.pc++ // Skip the reserved one byte.
		c.emit(
			&OperationMemorySize{},
		)
	case wasm.OpcodeMemoryGrow:
		c.pc++ // Skip the reserved one byte.
		c.emit(
			&OperationMemoryGrow{},
		)
	case wasm.OpcodeI32Const:
		val, num, err := leb128.DecodeInt32(bytes.NewReader(c.body[c.pc+1:]))
		if err != nil {
			return fmt.Errorf("reading i32.const value: %v", err)
		}
		c.pc += num
		c.emit(
			&OperationConstI32{Value: uint32(val)},
		)
	case wasm.OpcodeI64Const:
		val, num, err := leb128.DecodeInt64(bytes.NewReader(c.body[c.pc+1:]))
		if err != nil {
			return fmt.Errorf("reading i64.const value: %v", err)
		}
		c.pc += num
		c.emit(
			&OperationConstI64{Value: uint64(val)},
		)
	case wasm.OpcodeF32Const:
		v := math.Float32frombits(binary.LittleEndian.Uint32(c.body[c.pc+1:]))
		c.pc += 4
		c.emit(
			&OperationConstF32{Value: v},
		)
	case wasm.OpcodeF64Const:
		v := math.Float64frombits(binary.LittleEndian.Uint64(c.body[c.pc+1:]))
		c.pc += 8
		c.emit(
			&OperationConstF64{Value: v},
		)
	case wasm.OpcodeI32Eqz:
		c.emit(
			&OperationEqz{Type: UnsignedInt32},
		)
	case wasm.OpcodeI32Eq:
		c.emit(
			&OperationEq{Type: UnsignedTypeI32},
		)
	case wasm.OpcodeI32Ne:
		c.emit(
			&OperationNe{Type: UnsignedTypeI32},
		)
	case wasm.OpcodeI32LtS:
		c.emit(
			&OperationLt{Type: SignedTypeInt32},
		)
	case wasm.OpcodeI32LtU:
		c.emit(
			&OperationLt{Type: SignedTypeUint32},
		)
	case wasm.OpcodeI32GtS:
		c.emit(
			&OperationGt{Type: SignedTypeInt32},
		)
	case wasm.OpcodeI32GtU:
		c.emit(
			&OperationGt{Type: SignedTypeUint32},
		)
	case wasm.OpcodeI32LeS:
		c.emit(
			&OperationLe{Type: SignedTypeInt32},
		)
	case wasm.OpcodeI32LeU:
		c.emit(
			&OperationLe{Type: SignedTypeUint32},
		)
	case wasm.OpcodeI32GeS:
		c.emit(
			&OperationGe{Type: SignedTypeInt32},
		)
	case wasm.OpcodeI32GeU:
		c.emit(
			&OperationGe{Type: SignedTypeUint32},
		)
	case wasm.OpcodeI64Eqz:
		c.emit(
			&OperationEqz{Type: UnsignedInt64},
		)
	case wasm.OpcodeI64Eq:
		c.emit(
			&OperationEq{Type: UnsignedTypeI64},
		)
	case wasm.OpcodeI64Ne:
		c.emit(
			&OperationNe{Type: UnsignedTypeI64},
		)
	case wasm.OpcodeI64LtS:
		c.emit(
			&OperationLt{Type: SignedTypeInt64},
		)
	case wasm.OpcodeI64LtU:
		c.emit(
			&OperationLt{Type: SignedTypeUint64},
		)
	case wasm.OpcodeI64GtS:
		c.emit(
			&OperationGt{Type: SignedTypeInt64},
		)
	case wasm.OpcodeI64GtU:
		c.emit(
			&OperationGt{Type: SignedTypeUint64},
		)
	case wasm.OpcodeI64LeS:
		c.emit(
			&OperationLe{Type: SignedTypeInt64},
		)
	case wasm.OpcodeI64LeU:
		c.emit(
			&OperationLe{Type: SignedTypeUint64},
		)
	case wasm.OpcodeI64GeS:
		c.emit(
			&OperationGe{Type: SignedTypeInt64},
		)
	case wasm.OpcodeI64GeU:
		c.emit(
			&OperationGe{Type: SignedTypeUint64},
		)
	case wasm.OpcodeF32Eq:
		c.emit(
			&OperationEq{Type: UnsignedTypeF32},
		)
	case wasm.OpcodeF32Ne:
		c.emit(
			&OperationNe{Type: UnsignedTypeF32},
		)
	case wasm.OpcodeF32Lt:
		c.emit(
			&OperationLt{Type: SignedTypeFloat32},
		)
	case wasm.OpcodeF32Gt:
		c.emit(
			&OperationGt{Type: SignedTypeFloat32},
		)
	case wasm.OpcodeF32Le:
		c.emit(
			&OperationLe{Type: SignedTypeFloat32},
		)
	case wasm.OpcodeF32Ge:
		c.emit(
			&OperationGe{Type: SignedTypeFloat32},
		)
	case wasm.OpcodeF64Eq:
		c.emit(
			&OperationEq{Type: UnsignedTypeF64},
		)
	case wasm.OpcodeF64Ne:
		c.emit(
			&OperationNe{Type: UnsignedTypeF64},
		)
	case wasm.OpcodeF64Lt:
		c.emit(
			&OperationLt{Type: SignedTypeFloat64},
		)
	case wasm.OpcodeF64Gt:
		c.emit(
			&OperationGt{Type: SignedTypeFloat64},
		)
	case wasm.OpcodeF64Le:
		c.emit(
			&OperationLe{Type: SignedTypeFloat64},
		)
	case wasm.OpcodeF64Ge:
		c.emit(
			&OperationGe{Type: SignedTypeFloat64},
		)
	case wasm.OpcodeI32Clz:
		c.emit(
			&OperationClz{Type: UnsignedInt32},
		)
	case wasm.OpcodeI32Ctz:
		c.emit(
			&OperationCtz{Type: UnsignedInt32},
		)
	case wasm.OpcodeI32Popcnt:
		c.emit(
			&OperationPopcnt{Type: UnsignedInt32},
		)
	case wasm.OpcodeI32Add:
		c.emit(
			&OperationAdd{Type: UnsignedTypeI32},
		)
	case wasm.OpcodeI32Sub:
		c.emit(
			&OperationSub{Type: UnsignedTypeI32},
		)
	case wasm.OpcodeI32Mul:
		c.emit(
			&OperationMul{Type: UnsignedTypeI32},
		)
	case wasm.OpcodeI32DivS:
		c.emit(
			&OperationDiv{Type: SignedTypeInt32},
		)
	case wasm.OpcodeI32DivU:
		c.emit(
			&OperationDiv{Type: SignedTypeUint32},
		)
	case wasm.OpcodeI32RemS:
		c.emit(
			&OperationRem{Type: SignedInt32},
		)
	case wasm.OpcodeI32RemU:
		c.emit(
			&OperationRem{Type: SignedUint32},
		)
	case wasm.OpcodeI32And:
		c.emit(
			&OperationAnd{Type: UnsignedInt32},
		)
	case wasm.OpcodeI32Or:
		c.emit(
			&OperationOr{Type: UnsignedInt32},
		)
	case wasm.OpcodeI32Xor:
		c.emit(
			&OperationXor{Type: UnsignedInt64},
		)
	case wasm.OpcodeI32Shl:
		c.emit(
			&OperationShl{Type: UnsignedInt32},
		)
	case wasm.OpcodeI32ShrS:
		c.emit(
			&OperationShr{Type: SignedInt32},
		)
	case wasm.OpcodeI32ShrU:
		c.emit(
			&OperationShr{Type: SignedUint32},
		)
	case wasm.OpcodeI32Rotl:
		c.emit(
			&OperationRotl{Type: UnsignedInt32},
		)
	case wasm.OpcodeI32Rotr:
		c.emit(
			&OperationRotr{Type: UnsignedInt32},
		)
	case wasm.OpcodeI64Clz:
		c.emit(
			&OperationClz{Type: UnsignedInt64},
		)
	case wasm.OpcodeI64Ctz:
		c.emit(
			&OperationCtz{Type: UnsignedInt64},
		)
	case wasm.OpcodeI64Popcnt:
		c.emit(
			&OperationPopcnt{Type: UnsignedInt64},
		)
	case wasm.OpcodeI64Add:
		c.emit(
			&OperationAdd{Type: UnsignedTypeI64},
		)
	case wasm.OpcodeI64Sub:
		c.emit(
			&OperationSub{Type: UnsignedTypeI64},
		)
	case wasm.OpcodeI64Mul:
		c.emit(
			&OperationMul{Type: UnsignedTypeI64},
		)
	case wasm.OpcodeI64DivS:
		c.emit(
			&OperationDiv{Type: SignedTypeInt64},
		)
	case wasm.OpcodeI64DivU:
		c.emit(
			&OperationDiv{Type: SignedTypeUint64},
		)
	case wasm.OpcodeI64RemS:
		c.emit(
			&OperationRem{Type: SignedInt64},
		)
	case wasm.OpcodeI64RemU:
		c.emit(
			&OperationRem{Type: SignedUint64},
		)
	case wasm.OpcodeI64And:
		c.emit(
			&OperationAnd{Type: UnsignedInt64},
		)
	case wasm.OpcodeI64Or:
		c.emit(
			&OperationOr{Type: UnsignedInt64},
		)
	case wasm.OpcodeI64Xor:
		c.emit(
			&OperationXor{Type: UnsignedInt64},
		)
	case wasm.OpcodeI64Shl:
		c.emit(
			&OperationShl{Type: UnsignedInt64},
		)
	case wasm.OpcodeI64ShrS:
		c.emit(
			&OperationShr{Type: SignedInt64},
		)
	case wasm.OpcodeI64ShrU:
		c.emit(
			&OperationShr{Type: SignedUint64},
		)
	case wasm.OpcodeI64Rotl:
		c.emit(
			&OperationRotl{Type: UnsignedInt64},
		)
	case wasm.OpcodeI64Rotr:
		c.emit(
			&OperationRotr{Type: UnsignedInt64},
		)
	case wasm.OpcodeF32Abs:
		c.emit(
			&OperationAbs{Type: Float32},
		)
	case wasm.OpcodeF32Neg:
		c.emit(
			&OperationNeg{Type: Float32},
		)
	case wasm.OpcodeF32Ceil:
		c.emit(
			&OperationCeil{Type: Float32},
		)
	case wasm.OpcodeF32Floor:
		c.emit(
			&OperationFloor{Type: Float32},
		)
	case wasm.OpcodeF32Trunc:
		c.emit(
			&OperationTrunc{Type: Float32},
		)
	case wasm.OpcodeF32Nearest:
		c.emit(
			&OperationNearest{Type: Float32},
		)
	case wasm.OpcodeF32Sqrt:
		c.emit(
			&OperationSqrt{Type: Float32},
		)
	case wasm.OpcodeF32Add:
		c.emit(
			&OperationAdd{Type: UnsignedTypeF32},
		)
	case wasm.OpcodeF32Sub:
		c.emit(
			&OperationSub{Type: UnsignedTypeF32},
		)
	case wasm.OpcodeF32Mul:
		c.emit(
			&OperationMul{Type: UnsignedTypeF32},
		)
	case wasm.OpcodeF32Div:
		c.emit(
			&OperationDiv{Type: SignedTypeFloat32},
		)
	case wasm.OpcodeF32Min:
		c.emit(
			&OperationMin{Type: Float32},
		)
	case wasm.OpcodeF32Max:
		c.emit(
			&OperationMax{Type: Float32},
		)
	case wasm.OpcodeF32Copysign:
		c.emit(
			&OperationCopysign{Type: Float32},
		)
	case wasm.OpcodeF64Abs:
		c.emit(
			&OperationAbs{Type: Float64},
		)
	case wasm.OpcodeF64Neg:
		c.emit(
			&OperationNeg{Type: Float64},
		)
	case wasm.OpcodeF64Ceil:
		c.emit(
			&OperationCeil{Type: Float64},
		)
	case wasm.OpcodeF64Floor:
		c.emit(
			&OperationFloor{Type: Float64},
		)
	case wasm.OpcodeF64Trunc:
		c.emit(
			&OperationTrunc{Type: Float64},
		)
	case wasm.OpcodeF64Nearest:
		c.emit(
			&OperationNearest{Type: Float64},
		)
	case wasm.OpcodeF64Sqrt:
		c.emit(
			&OperationSqrt{Type: Float64},
		)
	case wasm.OpcodeF64Add:
		c.emit(
			&OperationAdd{Type: UnsignedTypeF64},
		)
	case wasm.OpcodeF64Sub:
		c.emit(
			&OperationSub{Type: UnsignedTypeF64},
		)
	case wasm.OpcodeF64Mul:
		c.emit(
			&OperationMul{Type: UnsignedTypeF64},
		)
	case wasm.OpcodeF64Div:
		c.emit(
			&OperationDiv{Type: SignedTypeFloat64},
		)
	case wasm.OpcodeF64Min:
		c.emit(
			&OperationMin{Type: Float64},
		)
	case wasm.OpcodeF64Max:
		c.emit(
			&OperationMax{Type: Float64},
		)
	case wasm.OpcodeF64Copysign:
		c.emit(
			&OperationCopysign{Type: Float64},
		)
	case wasm.OpcodeI32WrapI64:
		c.emit(
			&OperationI32WrapFromI64{},
		)
	case wasm.OpcodeI32TruncF32S:
		c.emit(
			&OperationITruncFromF{InputType: Float32, OutputType: SignedInt32},
		)
	case wasm.OpcodeI32TruncF32U:
		c.emit(
			&OperationITruncFromF{InputType: Float32, OutputType: SignedUint32},
		)
	case wasm.OpcodeI32TruncF64S:
		c.emit(
			&OperationITruncFromF{InputType: Float64, OutputType: SignedInt32},
		)
	case wasm.OpcodeI32TruncF64U:
		c.emit(
			&OperationITruncFromF{InputType: Float64, OutputType: SignedUint32},
		)
	case wasm.OpcodeI64ExtendI32S:
		c.emit(
			&OperationExtend{Signed: true},
		)
	case wasm.OpcodeI64ExtendI32U:
		c.emit(
			&OperationExtend{Signed: false},
		)
	case wasm.OpcodeI64TruncF32S:
		c.emit(
			&OperationITruncFromF{InputType: Float32, OutputType: SignedInt64},
		)
	case wasm.OpcodeI64TruncF32U:
		c.emit(
			&OperationITruncFromF{InputType: Float32, OutputType: SignedUint64},
		)
	case wasm.OpcodeI64TruncF64S:
		c.emit(
			&OperationITruncFromF{InputType: Float64, OutputType: SignedInt64},
		)
	case wasm.OpcodeI64TruncF64U:
		c.emit(
			&OperationITruncFromF{InputType: Float64, OutputType: SignedUint64},
		)
	case wasm.OpcodeF32ConvertI32s:
		c.emit(
			&OperationFConvertFromI{InputType: SignedInt32, OutputType: Float32},
		)
	case wasm.OpcodeF32ConvertI32U:
		c.emit(
			&OperationFConvertFromI{InputType: SignedUint32, OutputType: Float32},
		)
	case wasm.OpcodeF32ConvertI64S:
		c.emit(
			&OperationFConvertFromI{InputType: SignedInt64, OutputType: Float32},
		)
	case wasm.OpcodeF32ConvertI64U:
		c.emit(
			&OperationFConvertFromI{InputType: SignedUint64, OutputType: Float32},
		)
	case wasm.OpcodeF32DemoteF64:
		c.emit(
			&OperationF32DemoteFromF64{},
		)
	case wasm.OpcodeF64ConvertI32S:
		c.emit(
			&OperationFConvertFromI{InputType: SignedInt32, OutputType: Float64},
		)
	case wasm.OpcodeF64ConvertI32U:
		c.emit(
			&OperationFConvertFromI{InputType: SignedUint32, OutputType: Float64},
		)
	case wasm.OpcodeF64ConvertI64S:
		c.emit(
			&OperationFConvertFromI{InputType: SignedInt64, OutputType: Float64},
		)
	case wasm.OpcodeF64ConvertI64U:
		c.emit(
			&OperationFConvertFromI{InputType: SignedUint64, OutputType: Float64},
		)
	case wasm.OpcodeF64PromoteF32:
		c.emit(
			&OperationF64PromoteFromF32{},
		)
	case wasm.OpcodeI32ReinterpretF32:
		c.emit(
			&OperationI32ReinterpretFromF32{},
		)
	case wasm.OpcodeI64ReinterpretF64:
		c.emit(
			&OperationI64ReinterpretFromF64{},
		)
	case wasm.OpcodeF32ReinterpretI32:
		c.emit(
			&OperationF32ReinterpretFromI32{},
		)
	case wasm.OpcodeF64ReinterpretI64:
		c.emit(
			&OperationF64ReinterpretFromI64{},
		)
	case wasm.OpcodeI32Extend8S:
		c.emit(
			&OperationSignExtend32From8{},
		)
	case wasm.OpcodeI32Extend16S:
		c.emit(
			&OperationSignExtend32From16{},
		)
	case wasm.OpcodeI64Extend8S:
		c.emit(
			&OperationSignExtend64From8{},
		)
	case wasm.OpcodeI64Extend16S:
		c.emit(
			&OperationSignExtend64From16{},
		)
	case wasm.OpcodeI64Extend32S:
		c.emit(
			&OperationSignExtend64From32{},
		)
	case wasm.OpcodeRefFunc:
		c.pc++
		index, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc:]))
		if err != nil {
			return fmt.Errorf("failed to read function index for ref.func: %v", err)
		}
		c.pc += num - 1
		c.emit(
			&OperationRefFunc{FunctionIndex: index},
		)
	case wasm.OpcodeRefNull:
		c.pc++ // Skip the type of reftype as every ref value is opaque pointer.
		c.emit(
			&OperationConstI64{Value: 0},
		)
	case wasm.OpcodeRefIsNull:
		// Simply compare the opaque pointer (i64) with zero.
		c.emit(
			&OperationEqz{Type: UnsignedInt64},
		)
	case wasm.OpcodeTableGet:
		c.pc++
		tableIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc:]))
		if err != nil {
			return fmt.Errorf("failed to read function index for table.get: %v", err)
		}
		c.pc += num - 1
		c.emit(
			&OperationTableGet{TableIndex: tableIndex},
		)
	case wasm.OpcodeTableSet:
		c.pc++
		tableIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc:]))
		if err != nil {
			return fmt.Errorf("failed to read function index for table.set: %v", err)
		}
		c.pc += num - 1
		c.emit(
			&OperationTableSet{TableIndex: tableIndex},
		)
	case wasm.OpcodeMiscPrefix:
		c.pc++
		switch miscOp := c.body[c.pc]; miscOp {
		case wasm.OpcodeMiscI32TruncSatF32S:
			c.emit(
				&OperationITruncFromF{InputType: Float32, OutputType: SignedInt32, NonTrapping: true},
			)
		case wasm.OpcodeMiscI32TruncSatF32U:
			c.emit(
				&OperationITruncFromF{InputType: Float32, OutputType: SignedUint32, NonTrapping: true},
			)
		case wasm.OpcodeMiscI32TruncSatF64S:
			c.emit(
				&OperationITruncFromF{InputType: Float64, OutputType: SignedInt32, NonTrapping: true},
			)
		case wasm.OpcodeMiscI32TruncSatF64U:
			c.emit(
				&OperationITruncFromF{InputType: Float64, OutputType: SignedUint32, NonTrapping: true},
			)
		case wasm.OpcodeMiscI64TruncSatF32S:
			c.emit(
				&OperationITruncFromF{InputType: Float32, OutputType: SignedInt64, NonTrapping: true},
			)
		case wasm.OpcodeMiscI64TruncSatF32U:
			c.emit(
				&OperationITruncFromF{InputType: Float32, OutputType: SignedUint64, NonTrapping: true},
			)
		case wasm.OpcodeMiscI64TruncSatF64S:
			c.emit(
				&OperationITruncFromF{InputType: Float64, OutputType: SignedInt64, NonTrapping: true},
			)
		case wasm.OpcodeMiscI64TruncSatF64U:
			c.emit(
				&OperationITruncFromF{InputType: Float64, OutputType: SignedUint64, NonTrapping: true},
			)
		case wasm.OpcodeMiscMemoryInit:
			dataIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {
				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num + 1 // +1 to skip the memory index which is fixed to zero.
			c.emit(
				&OperationMemoryInit{DataIndex: dataIndex},
			)
			c.result.NeedsAccessToDataInstances = true
		case wasm.OpcodeMiscDataDrop:
			dataIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {
				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num
			c.emit(
				&OperationDataDrop{DataIndex: dataIndex},
			)
			c.result.NeedsAccessToDataInstances = true
		case wasm.OpcodeMiscMemoryCopy:
			c.pc += 2 // +2 to skip two memory indexes which are fixed to zero.
			c.emit(
				&OperationMemoryCopy{},
			)
		case wasm.OpcodeMiscMemoryFill:
			c.pc += 1 // +1 to skip the memory index which is fixed to zero.
			c.emit(
				&OperationMemoryFill{},
			)
		case wasm.OpcodeMiscTableInit:
			elemIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {
				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num
			// Read table index which is fixed to zero currently.
			tableIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {
				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num
			c.emit(
				&OperationTableInit{ElemIndex: elemIndex, TableIndex: tableIndex},
			)
			c.result.NeedsAccessToElementInstances = true
		case wasm.OpcodeMiscElemDrop:
			elemIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {
				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num
			c.emit(
				&OperationElemDrop{ElemIndex: elemIndex},
			)
			c.result.NeedsAccessToElementInstances = true
		case wasm.OpcodeMiscTableCopy:
			// Read the source table index.
			dst, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {
				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num
			// Read the destination table index.
			src, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {

				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num
			c.emit(
				&OperationTableCopy{SrcTableIndex: src, DstTableIndex: dst},
			)
			c.result.NeedsAccessToElementInstances = true
		case wasm.OpcodeMiscTableGrow:
			// Read the source table index.
			tableIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {
				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num
			c.emit(
				&OperationTableGrow{TableIndex: tableIndex},
			)
			c.result.NeedsAccessToElementInstances = true
		case wasm.OpcodeMiscTableSize:
			// Read the source table index.
			tableIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {
				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num
			c.emit(
				&OperationTableSize{TableIndex: tableIndex},
			)
			c.result.NeedsAccessToElementInstances = true
		case wasm.OpcodeMiscTableFill:
			// Read the source table index.
			tableIndex, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
			if err != nil {
				return fmt.Errorf("reading i32.const value: %v", err)
			}
			c.pc += num
			c.emit(
				&OperationTableFill{TableIndex: tableIndex},
			)
			c.result.NeedsAccessToElementInstances = true
		default:
			return fmt.Errorf("unsupported misc instruction in wazeroir: 0x%x", op)
		}
	case wasm.OpcodeVecPrefix:
		c.pc++
		switch miscOp := c.body[c.pc]; miscOp {
		case wasm.OpcodeVecV128Const:
			c.pc++
			lo := binary.LittleEndian.Uint64(c.body[c.pc : c.pc+8])
			c.pc += 8
			hi := binary.LittleEndian.Uint64(c.body[c.pc : c.pc+8])
			c.emit(
				&OperationConstV128{Lo: lo, Hi: hi},
			)
			c.pc += 7
		case wasm.OpcodeVecI32x4Add:
			c.emit(
				&OperationI32x4Add{},
			)
		case wasm.OpcodeVecI64x2Add:
			c.emit(
				&OperationI64x2Add{},
			)
		default:
			return fmt.Errorf("unsupported vector instruction in wazeroir: 0x%x", op)
		}
	default:
		return fmt.Errorf("unsupported instruction in wazeroir: 0x%x", op)
	}

	// Move the program counter to point to the next instruction.
	c.pc++
	return nil
}

func (c *compiler) nextID() (id uint32) {
	id = c.currentID + 1
	c.currentID++
	return
}

func (c *compiler) applyToStack(opcode wasm.Opcode) (*uint32, error) {
	var index uint32
	var ptr *uint32
	switch opcode {
	case
		// These are the opcodes that is coupled with "index"　immediate
		// and it DOES affect the signature of opcode.
		wasm.OpcodeCall,
		wasm.OpcodeCallIndirect,
		wasm.OpcodeLocalGet,
		wasm.OpcodeLocalSet,
		wasm.OpcodeLocalTee,
		wasm.OpcodeGlobalGet,
		wasm.OpcodeGlobalSet:
		// Assumes that we are at the opcode now so skip it before read immediates.
		v, num, err := leb128.DecodeUint32(bytes.NewReader(c.body[c.pc+1:]))
		if err != nil {
			return nil, fmt.Errorf("reading immediates: %w", err)
		}
		c.pc += num
		index = v
		ptr = &index
	default:
		// Note that other opcodes are free of index
		// as it doesn't affect the signature of opt code.
		// In other words, the "index" argument of wasmOpcodeSignature
		// is ignored there.
	}

	if c.unreachableState.on {
		return ptr, nil
	}

	// Retrieve the signature of the opcode.
	s, err := c.wasmOpcodeSignature(opcode, index)
	if err != nil {
		return nil, err
	}

	// Manipulate the stack according to the signature.
	// Note that the following algorithm assumes that
	// the unknown type is unique in the signature,
	// and is determined by the actual type on the stack.
	// The determined type is stored in this typeParam.
	var typeParam *UnsignedType
	for i := range s.in {
		want := s.in[len(s.in)-1-i]
		actual := c.stackPop()
		if want == UnsignedTypeUnknown && typeParam != nil {
			want = *typeParam
		} else if want == UnsignedTypeUnknown {
			want = actual
			typeParam = &actual
		}
		if want != actual {
			return nil, fmt.Errorf("input signature mismatch: want %s but have %s", want, actual)
		}
	}

	for _, target := range s.out {
		if target == UnsignedTypeUnknown && typeParam == nil {
			return nil, fmt.Errorf("cannot determine type of unknown result")
		} else if target == UnsignedTypeUnknown {
			c.stackPush(*typeParam)
		} else {
			c.stackPush(target)
		}
	}

	return ptr, nil
}

func (c *compiler) stackPop() (ret UnsignedType) {
	// No need to check stack bound
	// as we can assume that all the operations
	// are valid thanks to validateFunction
	// at module validation phase.
	ret = c.stack[len(c.stack)-1]
	c.stack = c.stack[:len(c.stack)-1]
	return
}

func (c *compiler) stackPush(ts ...UnsignedType) {
	c.stack = append(c.stack, ts...)
}

// emit adds the operations into the result.
func (c *compiler) emit(ops ...Operation) {
	if !c.unreachableState.on {
		for _, op := range ops {
			switch o := op.(type) {
			case *OperationDrop:
				// If the drop range is nil,
				// we could remove such operations.
				// That happens when drop operation is unnecessary.
				// i.e. when there's no need to adjust stack before jmp.
				if o.Depth == nil {
					continue
				}
			}
			c.result.Operations = append(c.result.Operations, op)
			if buildoptions.IsDebugMode {
				fmt.Printf("emitting ")
				formatOperation(os.Stdout, op)
			}
		}
	}
}

// Emit const expression with default values of the given type.
func (c *compiler) emitDefaultValue(t wasm.ValueType) {
	switch t {
	case wasm.ValueTypeI32:
		c.stackPush(UnsignedTypeI32)
		c.emit(&OperationConstI32{Value: 0})
	case wasm.ValueTypeI64, wasm.ValueTypeExternref, wasm.ValueTypeFuncref:
		c.stackPush(UnsignedTypeI64)
		c.emit(&OperationConstI64{Value: 0})
	case wasm.ValueTypeF32:
		c.stackPush(UnsignedTypeF32)
		c.emit(&OperationConstF32{Value: 0})
	case wasm.ValueTypeF64:
		c.stackPush(UnsignedTypeF64)
		c.emit(&OperationConstF64{Value: 0})
	case wasm.ValueTypeV128:
		c.stackPush(UnsignedTypeI64)
		c.emit(&OperationConstI64{Value: 0})
		c.stackPush(UnsignedTypeI64)
		c.emit(&OperationConstI64{Value: 0})
	}
}

// Returns the "depth" (starting from top of the stack)
// of the n-th local.
func (c *compiler) localDepth(index wasm.Index) int {
	height, ok := c.localIndexToStackHeight[index]
	if !ok {
		panic("BUG")
	}
	return int(len(c.stack)) - 1 - int(height)
}

func (c *compiler) localType(index wasm.Index) (t wasm.ValueType) {
	if params := uint32(len(c.sig.Params)); index < params {
		t = c.sig.Params[index]
	} else {
		t = c.localTypes[index-params]
	}
	return
}

// getFrameDropRange returns the range (starting from top of the stack) that spans across the stack. The range is
// supposed to be dropped from the stack when the given frame exists or branch into it.
//
// * frame is the control frame which the call-site is trying to branch into or exit.
// * isEnd true if the call-site is handling wasm.OpcodeEnd.
func (c *compiler) getFrameDropRange(frame *controlFrame, isEnd bool) *InclusiveRange {
	var start int
	if !isEnd && frame.kind == controlFrameKindLoop {
		// If this is not End and the call-site is trying to branch into the Loop control frame,
		// we have to start executing from the beginning of the loop block.
		// Therefore, we have to pass the inputs to the frame.
		start = frame.blockType.ParamNumInUint64
	} else {
		start = frame.blockType.ResultNumInUint64
	}
	var end int
	if frame.kind == controlFrameKindFunction {
		// On the function return, we eliminate all the contents on the stack
		// including locals (existing below of frame.originalStackLen)
		end = len(c.stack) - 1
	} else {
		end = len(c.stack) - 1 - frame.originalStackLenWithoutParam
	}
	if start <= end {
		return &InclusiveRange{Start: start, End: end}
	}
	return nil
}

func (c *compiler) readMemoryImmediate(tag string) (*MemoryImmediate, error) {
	r := bytes.NewReader(c.body[c.pc+1:])
	alignment, num, err := leb128.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("reading alignment for %s: %w", tag, err)
	}
	c.pc += num
	offset, num, err := leb128.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("reading offset for %s: %w", tag, err)
	}
	c.pc += num
	return &MemoryImmediate{Offset: offset, Alignment: alignment}, nil
}
