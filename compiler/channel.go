package compiler

// This file lowers channel operations (make/send/recv/close) to runtime calls
// or pseudo-operations that are lowered during goroutine lowering.

import (
	"go/types"

	"github.com/tinygo-org/tinygo/compiler/llvmutil"
	"golang.org/x/tools/go/ssa"
	"tinygo.org/x/go-llvm"
)

func (c *Compiler) emitMakeChan(frame *Frame, expr *ssa.MakeChan) llvm.Value {
	elementSize := c.targetData.TypeAllocSize(c.getLLVMType(expr.Type().(*types.Chan).Elem()))
	elementSizeValue := llvm.ConstInt(c.uintptrType, elementSize, false)
	bufSize := c.getValue(frame, expr.Size)
	return c.createRuntimeCall("chanMake", []llvm.Value{elementSizeValue, bufSize}, "")
}

// emitChanSend emits a pseudo chan send operation. It is lowered to the actual
// channel send operation during goroutine lowering.
func (c *Compiler) emitChanSend(frame *Frame, instr *ssa.Send) {
	ch := c.getValue(frame, instr.Chan)
	chanValue := c.getValue(frame, instr.X)

	// store value-to-send
	valueType := c.getLLVMType(instr.X.Type())
	valueAlloca, valueAllocaCast, valueAllocaSize := c.createTemporaryAlloca(valueType, "chan.value")
	c.builder.CreateStore(chanValue, valueAlloca)

	// Do the send.
	c.createRuntimeCall("chanSend", []llvm.Value{ch, valueAllocaCast}, "")

	// End the lifetime of the alloca.
	// This also works around a bug in CoroSplit, at least in LLVM 8:
	// https://bugs.llvm.org/show_bug.cgi?id=41742
	c.emitLifetimeEnd(valueAllocaCast, valueAllocaSize)
}

// emitChanRecv emits a pseudo chan receive operation. It is lowered to the
// actual channel receive operation during goroutine lowering.
func (c *Compiler) emitChanRecv(frame *Frame, unop *ssa.UnOp) llvm.Value {
	valueType := c.getLLVMType(unop.X.Type().(*types.Chan).Elem())
	ch := c.getValue(frame, unop.X)

	// Allocate memory to receive into.
	valueAlloca, valueAllocaCast, valueAllocaSize := c.createTemporaryAlloca(valueType, "chan.value")

	// Do the receive.
	commaOk := c.createRuntimeCall("chanRecv", []llvm.Value{ch, valueAllocaCast}, "")
	received := c.builder.CreateLoad(valueAlloca, "chan.received")
	c.emitLifetimeEnd(valueAllocaCast, valueAllocaSize)

	if unop.CommaOk {
		tuple := llvm.Undef(c.ctx.StructType([]llvm.Type{valueType, c.ctx.Int1Type()}, false))
		tuple = c.builder.CreateInsertValue(tuple, received, 0, "")
		tuple = c.builder.CreateInsertValue(tuple, commaOk, 1, "")
		return tuple
	} else {
		return received
	}
}

// emitChanClose closes the given channel.
func (c *Compiler) emitChanClose(frame *Frame, param ssa.Value) {
	ch := c.getValue(frame, param)
	c.createRuntimeCall("chanClose", []llvm.Value{ch}, "")
}

// emitSelect emits all IR necessary for a select statements. That's a
// non-trivial amount of code because select is very complex to implement.
func (c *Compiler) emitSelect(frame *Frame, expr *ssa.Select) llvm.Value {
	if len(expr.States) == 0 {
		// Shortcuts for some simple selects.
		llvmType := c.getLLVMType(expr.Type())
		if expr.Blocking {
			// Blocks forever:
			//     select {}
			c.createRuntimeCall("deadlock", nil, "")
			return llvm.Undef(llvmType)
		} else {
			// No-op:
			//     select {
			//     default:
			//     }
			retval := llvm.Undef(llvmType)
			retval = c.builder.CreateInsertValue(retval, llvm.ConstInt(c.intType, 0xffffffffffffffff, true), 0, "")
			return retval // {-1, false}
		}
	}

	// This code create a (stack-allocated) slice containing all the select
	// cases and then calls runtime.chanSelect to perform the actual select
	// statement.
	// Simple selects (blocking and with just one case) are already transformed
	// into regular chan operations during SSA construction so we don't have to
	// optimize such small selects.

	// Go through all the cases. Create the selectStates slice and and
	// determine the receive buffer size and alignment.
	recvbufSize := uint64(0)
	recvbufAlign := 0
	hasReceives := false
	var selectStates []llvm.Value
	chanSelectStateType := c.getLLVMRuntimeType("chanSelectState")
	for _, state := range expr.States {
		ch := c.getValue(frame, state.Chan)
		selectState := llvm.ConstNull(chanSelectStateType)
		selectState = c.builder.CreateInsertValue(selectState, ch, 0, "")
		switch state.Dir {
		case types.RecvOnly:
			// Make sure the receive buffer is big enough and has the correct alignment.
			llvmType := c.getLLVMType(state.Chan.Type().(*types.Chan).Elem())
			if size := c.targetData.TypeAllocSize(llvmType); size > recvbufSize {
				recvbufSize = size
			}
			if align := c.targetData.ABITypeAlignment(llvmType); align > recvbufAlign {
				recvbufAlign = align
			}
			hasReceives = true
		case types.SendOnly:
			// Store this value in an alloca and put a pointer to this alloca
			// in the send state.
			sendValue := c.getValue(frame, state.Send)
			alloca := llvmutil.CreateEntryBlockAlloca(c.builder, sendValue.Type(), "select.send.value")
			c.builder.CreateStore(sendValue, alloca)
			ptr := c.builder.CreateBitCast(alloca, c.i8ptrType, "")
			selectState = c.builder.CreateInsertValue(selectState, ptr, 1, "")
		default:
			panic("unreachable")
		}
		selectStates = append(selectStates, selectState)
	}

	// Create a receive buffer, where the received value will be stored.
	recvbuf := llvm.Undef(c.i8ptrType)
	if hasReceives {
		allocaType := llvm.ArrayType(c.ctx.Int8Type(), int(recvbufSize))
		recvbufAlloca, _, _ := c.createTemporaryAlloca(allocaType, "select.recvbuf.alloca")
		recvbufAlloca.SetAlignment(recvbufAlign)
		recvbuf = c.builder.CreateGEP(recvbufAlloca, []llvm.Value{
			llvm.ConstInt(c.ctx.Int32Type(), 0, false),
			llvm.ConstInt(c.ctx.Int32Type(), 0, false),
		}, "select.recvbuf")
	}

	// Create the states slice (allocated on the stack).
	statesAllocaType := llvm.ArrayType(chanSelectStateType, len(selectStates))
	statesAlloca, statesI8, statesSize := c.createTemporaryAlloca(statesAllocaType, "select.states.alloca")
	for i, state := range selectStates {
		// Set each slice element to the appropriate channel.
		gep := c.builder.CreateGEP(statesAlloca, []llvm.Value{
			llvm.ConstInt(c.ctx.Int32Type(), 0, false),
			llvm.ConstInt(c.ctx.Int32Type(), uint64(i), false),
		}, "")
		c.builder.CreateStore(state, gep)
	}
	statesPtr := c.builder.CreateGEP(statesAlloca, []llvm.Value{
		llvm.ConstInt(c.ctx.Int32Type(), 0, false),
		llvm.ConstInt(c.ctx.Int32Type(), 0, false),
	}, "select.states")
	statesLen := llvm.ConstInt(c.uintptrType, uint64(len(selectStates)), false)

	// Do the select in the runtime.
	var results llvm.Value
	if expr.Blocking {
		// Stack-allocate operation structures.
		// If these were simply created as a slice, they would heap-allocate.
		chBlockAllocaType := llvm.ArrayType(c.getLLVMRuntimeType("channelBlockedList"), len(selectStates))
		chBlockAlloca, chBlockAllocaPtr, chBlockSize := c.createTemporaryAlloca(chBlockAllocaType, "select.block.alloca")
		chBlockLen := llvm.ConstInt(c.uintptrType, uint64(len(selectStates)), false)
		chBlockPtr := c.builder.CreateGEP(chBlockAlloca, []llvm.Value{
			llvm.ConstInt(c.ctx.Int32Type(), 0, false),
			llvm.ConstInt(c.ctx.Int32Type(), 0, false),
		}, "select.block")

		results = c.createRuntimeCall("chanSelect", []llvm.Value{
			recvbuf,
			statesPtr, statesLen, statesLen, // []chanSelectState
			chBlockPtr, chBlockLen, chBlockLen, // []channelBlockList
		}, "select.result")

		// Terminate the lifetime of the operation structures.
		c.emitLifetimeEnd(chBlockAllocaPtr, chBlockSize)
	} else {
		results = c.createRuntimeCall("tryChanSelect", []llvm.Value{
			recvbuf,
			statesPtr, statesLen, statesLen, // []chanSelectState
		}, "select.result")
	}

	// Terminate the lifetime of the states alloca.
	c.emitLifetimeEnd(statesI8, statesSize)

	// The result value does not include all the possible received values,
	// because we can't load them in advance. Instead, the *ssa.Extract
	// instruction will treat a *ssa.Select specially and load it there inline.
	// Store the receive alloca in a sidetable until we hit this extract
	// instruction.
	if frame.selectRecvBuf == nil {
		frame.selectRecvBuf = make(map[*ssa.Select]llvm.Value)
	}
	frame.selectRecvBuf[expr] = recvbuf

	return results
}

// getChanSelectResult returns the special values from a *ssa.Extract expression
// when extracting a value from a select statement (*ssa.Select). Because
// *ssa.Select cannot load all values in advance, it does this later in the
// *ssa.Extract expression.
func (c *Compiler) getChanSelectResult(frame *Frame, expr *ssa.Extract) llvm.Value {
	if expr.Index == 0 {
		// index
		value := c.getValue(frame, expr.Tuple)
		index := c.builder.CreateExtractValue(value, expr.Index, "")
		if index.Type().IntTypeWidth() < c.intType.IntTypeWidth() {
			index = c.builder.CreateSExt(index, c.intType, "")
		}
		return index
	} else if expr.Index == 1 {
		// comma-ok
		value := c.getValue(frame, expr.Tuple)
		return c.builder.CreateExtractValue(value, expr.Index, "")
	} else {
		// Select statements are (index, ok, ...) where ... is a number of
		// received values, depending on how many receive statements there
		// are. They are all combined into one alloca (because only one
		// receive can proceed at a time) so we'll get that alloca, bitcast
		// it to the correct type, and dereference it.
		recvbuf := frame.selectRecvBuf[expr.Tuple.(*ssa.Select)]
		typ := llvm.PointerType(c.getLLVMType(expr.Type()), 0)
		ptr := c.builder.CreateBitCast(recvbuf, typ, "")
		return c.builder.CreateLoad(ptr, "")
	}
}
