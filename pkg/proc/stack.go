package proc

import (
	"bytes"
	"debug/dwarf"
	"encoding/binary"
	"errors"
	"fmt"
	"go/constant"
	"reflect"
	"strings"

	"github.com/go-delve/delve/pkg/dwarf/frame"
	"github.com/go-delve/delve/pkg/dwarf/op"
	"github.com/go-delve/delve/pkg/dwarf/reader"
	"github.com/go-delve/delve/pkg/logflags"
)

// This code is partly adapted from runtime.gentraceback in
// $GOROOT/src/runtime/traceback.go

// Stackframe represents a frame in a system stack.
//
// Each stack frame has two locations Current and Call.
//
// For the topmost stackframe Current and Call are the same location.
//
// For stackframes after the first Current is the location corresponding to
// the return address and Call is the location of the CALL instruction that
// was last executed on the frame. Note however that Call.PC is always equal
// to Current.PC, because finding the correct value for Call.PC would
// require disassembling each function in the stacktrace.
//
// For synthetic stackframes generated for inlined function calls Current.Fn
// is the function containing the inlining and Call.Fn in the inlined
// function.
type Stackframe struct {
	Current, Call Location

	// Frame registers.
	Regs op.DwarfRegisters
	// High address of the stack.
	stackHi uint64
	// Return address for this stack frame (as read from the stack frame itself).
	Ret uint64
	// Err is set if an error occurred during stacktrace
	Err error
	// SystemStack is true if this frame belongs to a system stack.
	SystemStack bool
	// Inlined is true if this frame is actually an inlined call.
	Inlined bool
	// hasInlines is true if this frame is a concrete function that is executing inlined calls (i.e. if there is at least one inlined call frame on top of this one).
	hasInlines bool
	// Bottom is true if this is the bottom of the stack
	Bottom bool

	// lastpc is a memory address guaranteed to belong to the last instruction
	// executed in this stack frame.
	// For the topmost stack frame this will be the same as Current.PC and
	// Call.PC, for other stack frames it will usually be Current.PC-1, but
	// could be different when inlined calls are involved in the stacktrace.
	// Note that this address isn't guaranteed to belong to the start of an
	// instruction and, for this reason, should not be propagated outside of
	// pkg/proc.
	// Use this value to determine active lexical scopes for the stackframe.
	lastpc uint64

	// closurePtr is the value of .closureptr, if present. This variable is
	// used to correlated range-over-func closure bodies with their enclosing
	// function.
	closurePtr int64

	// TopmostDefer is the defer that would be at the top of the stack when a
	// panic unwind would get to this call frame, in other words it's the first
	// deferred function that will  be called if the runtime unwinds past this
	// call frame.
	TopmostDefer *Defer

	// Defers is the list of functions deferred by this stack frame (so far).
	Defers []*Defer
}

// FrameOffset returns the address of the stack frame, absolute for system
// stack frames or as an offset from stackhi for goroutine stacks (a
// negative value).
func (frame *Stackframe) FrameOffset() int64 {
	if frame.SystemStack {
		return frame.Regs.CFA
	}
	return frame.Regs.CFA - int64(frame.stackHi)
}

// FramePointerOffset returns the value of the frame pointer, absolute for
// system stack frames or as an offset from stackhi for goroutine stacks (a
// negative value).
func (frame *Stackframe) FramePointerOffset() int64 {
	if frame.SystemStack {
		return int64(frame.Regs.BP())
	}
	return int64(frame.Regs.BP()) - int64(frame.stackHi)
}

// contains returns true if off is between CFA and SP
func (frame *Stackframe) contains(off int64) bool {
	p := uint64(off + int64(frame.stackHi))
	return frame.Regs.SP() < p && p <= uint64(frame.Regs.CFA)
}

// ThreadStacktrace returns the stack trace for thread.
// Note the locations in the array are return addresses not call addresses.
func ThreadStacktrace(tgt *Target, thread Thread, depth int) ([]Stackframe, error) {
	g, _ := GetG(thread)
	if g == nil {
		regs, err := thread.Registers()
		if err != nil {
			return nil, err
		}
		so := thread.BinInfo().PCToImage(regs.PC())
		dwarfRegs := *(thread.BinInfo().Arch.RegistersToDwarfRegisters(so.StaticBase, regs))
		dwarfRegs.ChangeFunc = thread.SetReg
		it := newStackIterator(tgt, thread.BinInfo(), thread.ProcessMemory(), dwarfRegs, 0, nil, 0)
		return it.stacktrace(depth)
	}
	return GoroutineStacktrace(tgt, g, depth, 0)
}

func goroutineStackIterator(tgt *Target, g *G, opts StacktraceOptions) (*stackIterator, error) {
	bi := g.variable.bi
	if g.Thread != nil {
		regs, err := g.Thread.Registers()
		if err != nil {
			return nil, err
		}
		so := bi.PCToImage(regs.PC())
		dwarfRegs := *(bi.Arch.RegistersToDwarfRegisters(so.StaticBase, regs))
		dwarfRegs.ChangeFunc = g.Thread.SetReg
		return newStackIterator(
			tgt, bi, g.variable.mem,
			dwarfRegs,
			g.stack.hi, g, opts), nil
	}
	so := g.variable.bi.PCToImage(g.PC)
	return newStackIterator(
		tgt, bi, g.variable.mem,
		bi.Arch.addrAndStackRegsToDwarfRegisters(so.StaticBase, g.PC, g.SP, g.BP, g.LR),
		g.stack.hi, g, opts), nil
}

type StacktraceOptions uint16

const (
	// StacktraceReadDefers requests a stacktrace decorated with deferred calls
	// for each frame.
	StacktraceReadDefers StacktraceOptions = 1 << iota

	// StacktraceSimple requests a stacktrace where no stack switches will be
	// attempted.
	StacktraceSimple

	// StacktraceG requests a stacktrace starting with the register
	// values saved in the runtime.g structure.
	StacktraceG
)

// GoroutineStacktrace returns the stack trace for a goroutine.
// Note the locations in the array are return addresses not call addresses.
func GoroutineStacktrace(tgt *Target, g *G, depth int, opts StacktraceOptions) ([]Stackframe, error) {
	it, err := goroutineStackIterator(tgt, g, opts)
	if err != nil {
		return nil, err
	}
	frames, err := it.stacktrace(depth)
	if err != nil {
		return nil, err
	}
	if opts&StacktraceReadDefers != 0 {
		g.readDefers(frames)
	}
	return frames, nil
}

// NullAddrError is an error for a null address.
type NullAddrError struct{}

func (n NullAddrError) Error() string {
	return "NULL address"
}

// stackIterator holds information
// required to iterate and walk the program
// stack.
type stackIterator struct {
	pc     uint64
	top    bool
	atend  bool
	sigret bool
	frame  Stackframe
	target *Target
	bi     *BinaryInfo
	mem    MemoryReadWriter
	err    error

	stackhi     uint64
	systemstack bool

	// regs is the register set for the current frame
	regs op.DwarfRegisters

	g                  *G     // the goroutine being stacktraced, nil if we are stacktracing a goroutine-less thread
	g0_sched_sp        uint64 // value of g0.sched.sp (see comments around its use)
	g0_sched_sp_loaded bool   // g0_sched_sp was loaded from g0

	count int

	opts StacktraceOptions
}

func newStackIterator(tgt *Target, bi *BinaryInfo, mem MemoryReadWriter, regs op.DwarfRegisters, stackhi uint64, g *G, opts StacktraceOptions) *stackIterator {
	systemstack := true
	if g != nil {
		systemstack = g.SystemStack
	}
	return &stackIterator{pc: regs.PC(), regs: regs, top: true, target: tgt, bi: bi, mem: mem, err: nil, atend: false, stackhi: stackhi, systemstack: systemstack, g: g, opts: opts}
}

// Next points the iterator to the next stack frame.
func (it *stackIterator) Next() bool {
	if it.err != nil || it.atend {
		return false
	}

	if logflags.Stack() {
		logger := logflags.StackLogger()
		w := &strings.Builder{}
		fmt.Fprintf(w, "current pc = %#x CFA = %#x FrameBase = %#x ", it.pc, it.regs.CFA, it.regs.FrameBase)
		for i := 0; i < it.regs.CurrentSize(); i++ {
			reg := it.regs.Reg(uint64(i))
			if reg == nil {
				continue
			}
			name, _, _ := it.bi.Arch.DwarfRegisterToString(i, reg)
			fmt.Fprintf(w, " %s = %#x", name, reg.Uint64Val)
		}
		logger.Debugf("%s", w.String())
	}

	callFrameRegs, ret, retaddr := it.advanceRegs()
	it.frame = it.newStackframe(ret, retaddr)

	if logflags.Stack() {
		logger := logflags.StackLogger()
		fnname := "?"
		if it.frame.Call.Fn != nil {
			fnname = it.frame.Call.Fn.Name
		}
		logger.Debugf("new frame %#x %s:%d at %s", it.frame.Call.PC, it.frame.Call.File, it.frame.Call.Line, fnname)
	}

	if it.frame.Current.Fn != nil && it.frame.Current.Fn.Name == "runtime.sigtrampgo" && it.target != nil {
		regs, err := it.readSigtrampgoContext()
		if err != nil {
			logflags.DebuggerLogger().Errorf("could not read runtime.sigtrampgo context: %v", err)
		} else {
			so := it.bi.PCToImage(regs.PC())
			regs.StaticBase = so.StaticBase
			it.pc = regs.PC()
			it.regs = *regs
			it.top = false
			if it.g != nil && it.g.ID != 0 {
				it.systemstack = !(it.regs.SP() >= it.g.stack.lo && it.regs.SP() < it.g.stack.hi)
			}
			logflags.StackLogger().Debugf("sigtramp context read")
			return true
		}
	}

	if it.opts&StacktraceSimple == 0 {
		if it.bi.Arch.switchStack(it, &callFrameRegs) {
			logflags.StackLogger().Debugf("stack switched")
			return true
		}
	}

	if it.frame.Ret <= 0 {
		it.atend = true
		return true
	}

	it.sigret = it.frame.Current.Fn != nil && it.frame.Current.Fn.Name == "runtime.sigpanic"
	it.top = false
	it.pc = it.frame.Ret
	it.regs = callFrameRegs
	return true
}

func (it *stackIterator) switchToGoroutineStack() error {
	if it.g == nil {
		return fmt.Errorf("nil goroutine when attempting to switch to goroutine stack")
	}
	it.systemstack = false
	it.top = false
	it.pc = it.g.PC
	it.regs.Reg(it.regs.SPRegNum).Uint64Val = it.g.SP
	it.regs.AddReg(it.regs.BPRegNum, op.DwarfRegisterFromUint64(it.g.BP))
	if it.bi.Arch.usesLR {
		lrReg := it.regs.Reg(it.regs.LRRegNum)
		if lrReg == nil {
			return fmt.Errorf("LR register is nil during stack switch")
		}
		lrReg.Uint64Val = it.g.LR
	}
	return nil
}

// Frame returns the frame the iterator is pointing at.
func (it *stackIterator) Frame() Stackframe {
	it.frame.Bottom = it.atend
	return it.frame
}

// Err returns the error encountered during stack iteration.
func (it *stackIterator) Err() error {
	return it.err
}

// frameBase calculates the frame base pseudo-register for DWARF for fn and
// the current frame.
func (it *stackIterator) frameBase(fn *Function) int64 {
	if fn.cu.image.Stripped() {
		return 0
	}
	dwarfTree, err := fn.cu.image.getDwarfTree(fn.offset)
	if err != nil {
		return 0
	}
	fb, _, _, _ := it.bi.Location(dwarfTree.Entry, dwarf.AttrFrameBase, it.pc, it.regs, it.mem)
	return fb
}

func (it *stackIterator) newStackframe(ret, retaddr uint64) Stackframe {
	if retaddr == 0 {
		it.err = NullAddrError{}
		return Stackframe{}
	}
	f, l, fn := it.bi.PCToLine(it.pc)
	if fn == nil {
		f = "?"
		l = -1
	} else {
		it.regs.FrameBase = it.frameBase(fn)
	}
	r := Stackframe{Current: Location{PC: it.pc, File: f, Line: l, Fn: fn}, Regs: it.regs, Ret: ret, stackHi: it.stackhi, SystemStack: it.systemstack, lastpc: it.pc}
	if r.Regs.Reg(it.regs.PCRegNum) == nil {
		r.Regs.AddReg(it.regs.PCRegNum, op.DwarfRegisterFromUint64(it.pc))
	}
	r.Call = r.Current
	if !it.top && r.Current.Fn != nil && it.pc != r.Current.Fn.Entry && !it.sigret {
		// if the return address is the entry point of the function that
		// contains it then this is some kind of fake return frame (for example
		// runtime.sigreturn) that didn't actually call the current frame,
		// attempting to get the location of the CALL instruction would just
		// obfuscate what's going on, since there is no CALL instruction.
		switch r.Current.Fn.Name {
		case "runtime.mstart", "runtime.systemstack_switch":
			// these frames are inserted by runtime.systemstack and there is no CALL
			// instruction to look for at pc - 1
		default:
			r.lastpc = it.pc - 1
			r.Call.File, r.Call.Line = r.Current.Fn.cu.lineInfo.PCToLine(r.Current.Fn.Entry, it.pc-1)
		}
	}
	if fn != nil && !fn.cu.image.Stripped() && !r.SystemStack && it.g != nil {
		dwarfTree, _ := fn.cu.image.getDwarfTree(fn.offset)
		if dwarfTree != nil {
			c := readLocalPtrVar(dwarfTree, goClosurePtr, it.target, it.bi, fn.cu.image, r.Regs, it.mem)
			if c != 0 {
				if c >= it.g.stack.lo && c < it.g.stack.hi {
					r.closurePtr = int64(c) - int64(it.g.stack.hi)
				} else {
					r.closurePtr = int64(c)
				}
			}
		}
	}
	return r
}

func (it *stackIterator) stacktrace(depth int) ([]Stackframe, error) {
	if depth < 0 {
		return nil, errors.New("negative maximum stack depth")
	}
	frames := make([]Stackframe, 0, depth+1)
	f := func(frame Stackframe) bool {
		frames = append(frames, frame)
		return len(frames) < depth+1
	}
	it.stacktraceFunc(f)
	if it.Err() != nil && len(frames) == 1 && it.g != nil && frames[0].SystemStack && (it.opts&StacktraceSimple == 0) {
		// If we can't continue from the first frame, and it was on a system stack
		// and we have a goroutine which we are allowed to switch to then switch
		// to it and continue the stacktrace from there.
		// This improves stacktraces produced on Windows by WER where the first
		// thread will be executing a system function from which we can't continue
		// to trace.
		// See #3824.
		it.err = nil
		it.opts |= StacktraceG
		it.stacktraceFunc(f)
	}

	if err := it.Err(); err != nil {
		if len(frames) == 0 {
			return nil, err
		}

		frames = append(frames, Stackframe{Err: err})

	}
	return frames, nil
}

func (it *stackIterator) stacktraceFunc(callback func(Stackframe) bool) {
	if it.opts&StacktraceG != 0 && it.g != nil {
		if err := it.switchToGoroutineStack(); err != nil {
			it.err = err
			return
		}
		it.top = true
	}
	for it.Next() {
		if !it.appendInlineCalls(callback, it.Frame()) {
			break
		}
	}
}

func (it *stackIterator) appendInlineCalls(callback func(Stackframe) bool, frame Stackframe) bool {
	if frame.Call.Fn == nil {
		it.count++
		return callback(frame)
	}
	if frame.Call.Fn.cu.lineInfo == nil {
		it.count++
		return callback(frame)
	}

	callpc := frame.Call.PC
	if it.count > 0 {
		callpc--
	}

	dwarfTree, err := frame.Call.Fn.cu.image.getDwarfTree(frame.Call.Fn.offset)
	if err != nil {
		it.count++
		return callback(frame)
	}

	for _, entry := range reader.InlineStack(dwarfTree, callpc) {
		frame.hasInlines = true
		fnname, okname := entry.Val(dwarf.AttrName).(string)
		fileidx, okfileidx := entry.Val(dwarf.AttrCallFile).(int64)
		line, okline := entry.Val(dwarf.AttrCallLine).(int64)

		if !okname || !okfileidx || !okline {
			break
		}
		var e *dwarf.Entry
		filepath, fileErr := frame.Current.Fn.cu.filePath(int(fileidx), e)
		if fileErr != nil {
			break
		}

		inlfn := &Function{Name: fnname, Entry: frame.Call.Fn.Entry, End: frame.Call.Fn.End, offset: entry.Offset, cu: frame.Call.Fn.cu}
		it.count++
		callback(Stackframe{
			Current: frame.Current,
			Call: Location{
				frame.Call.PC,
				frame.Call.File,
				frame.Call.Line,
				inlfn,
			},
			Regs:        frame.Regs,
			stackHi:     frame.stackHi,
			Ret:         frame.Ret,
			Err:         frame.Err,
			SystemStack: frame.SystemStack,
			Inlined:     true,
			lastpc:      frame.lastpc,
			closurePtr:  frame.closurePtr,
		})

		frame.Call.File = filepath
		frame.Call.Line = int(line)
	}

	it.count++
	return callback(frame)
}

// advanceRegs calculates the DwarfRegisters for a next stack frame
// (corresponding to it.pc).
//
// The computation uses the registers for the current stack frame (it.regs) and
// the corresponding Frame Descriptor Entry (FDE) retrieved from the DWARF info.
//
// The new set of registers is returned. it.regs is not updated, except for
// it.regs.CFA; the caller has to eventually switch it.regs when the iterator
// advances to the next frame.
func (it *stackIterator) advanceRegs() (callFrameRegs op.DwarfRegisters, ret uint64, retaddr uint64) {
	logger := logflags.StackLogger()

	fde, err := it.bi.frameEntries.FDEForPC(it.pc)
	var framectx *frame.FrameContext
	if _, nofde := err.(*frame.ErrNoFDEForPC); nofde {
		framectx = it.bi.Arch.fixFrameUnwindContext(nil, it.pc, it.bi)
	} else {
		fctxt, err := fde.EstablishFrame(it.pc)
		if err != nil {
			logger.Errorf("Error executing Frame Debug Entry for PC %x: %v", it.pc, err)
		}
		framectx = it.bi.Arch.fixFrameUnwindContext(fctxt, it.pc, it.bi)
	}

	logger.Debugf("advanceRegs at %#x", it.pc)

	cfareg, err := it.executeFrameRegRule(0, framectx.CFA, 0)
	if cfareg == nil {
		it.err = fmt.Errorf("CFA becomes undefined at PC %#x: %v", it.pc, err)
		return op.DwarfRegisters{}, 0, 0
	}
	if logflags.Stack() {
		logger.Debugf("\tCFA rule %s -> %#x", ruleString(&framectx.CFA, it.bi.Arch.RegnumToString), cfareg.Uint64Val)
	}
	it.regs.CFA = int64(cfareg.Uint64Val)

	callimage := it.bi.PCToImage(it.pc)

	callFrameRegs = op.DwarfRegisters{
		StaticBase: callimage.StaticBase,
		ByteOrder:  it.regs.ByteOrder,
		PCRegNum:   it.regs.PCRegNum,
		SPRegNum:   it.regs.SPRegNum,
		BPRegNum:   it.regs.BPRegNum,
		LRRegNum:   it.regs.LRRegNum,
	}

	// According to the standard the compiler should be responsible for emitting
	// rules for the RSP register so that it can then be used to calculate CFA,
	// however neither Go nor GCC do this.
	// In the following line we copy GDB's behaviour by assuming this is
	// implicit.
	// See also the comment in dwarf2_frame_default_init in
	// $GDB_SOURCE/dwarf2/frame.c
	callFrameRegs.AddReg(callFrameRegs.SPRegNum, cfareg)

	for i, regRule := range framectx.Regs {
		if logflags.Stack() {
			logger.Debugf("\t%s rule %s ", it.bi.Arch.RegnumToString(i), ruleString(&regRule, it.bi.Arch.RegnumToString))
		}
		reg, err := it.executeFrameRegRule(i, regRule, it.regs.CFA)
		if reg != nil {
			logger.Debugf("\t\t-> %#x", reg.Uint64Val)
		} else {
			logger.Debugf("\t\t-> nothing (%v)", err)
		}
		callFrameRegs.AddReg(i, reg)
		if i == framectx.RetAddrReg {
			if reg == nil {
				if err == nil {
					//lint:ignore ST1005 backwards compatibility
					err = fmt.Errorf("Undefined return address at %#x", it.pc)
				}
				it.err = err
			} else {
				ret = reg.Uint64Val
				// On systems which use a link register to store the return address of a function,
				// certain leaf functions may not have correct DWARF information present in the
				// .debug_frame FDE when unwinding after a fatal signal. This is due to the fact
				// that runtime.sigpanic inserts a frame to make it look like the function which
				// triggered the signal called runtime.sigpanic directly, making the value of the
				// link register unreliable. Instead, treat it as a non-leaf function and read the
				// return address from the stack. For more details, see:
				// https://github.com/golang/go/issues/63862#issuecomment-1802672629.
				if it.frame.Call.Fn != nil && it.frame.Call.Fn.Name == "runtime.sigpanic" && it.bi.Arch.usesLR {
					buf := make([]byte, 8)
					_, err := it.mem.ReadMemory(buf, uint64(it.regs.CFA))
					if err != nil {
						it.err = err
					}
					binary.Read(bytes.NewReader(buf), binary.LittleEndian, &ret)
				}
			}
			retaddr = uint64(it.regs.CFA + regRule.Offset)
		}
	}

	if it.bi.Arch.usesLR {
		if ret == 0 && it.regs.Reg(it.regs.LRRegNum) != nil {
			ret = it.regs.Reg(it.regs.LRRegNum).Uint64Val
		}
	}

	return callFrameRegs, ret, retaddr
}

func (it *stackIterator) executeFrameRegRule(regnum uint64, rule frame.DWRule, cfa int64) (*op.DwarfRegister, error) {
	switch rule.Rule {
	default:
		fallthrough
	case frame.RuleUndefined:
		return nil, nil
	case frame.RuleSameVal:
		if it.regs.Reg(regnum) == nil {
			return nil, nil
		}
		reg := *it.regs.Reg(regnum)
		return &reg, nil
	case frame.RuleOffset:
		return it.readRegisterAt(regnum, uint64(cfa+rule.Offset))
	case frame.RuleValOffset:
		return op.DwarfRegisterFromUint64(uint64(cfa + rule.Offset)), nil
	case frame.RuleRegister:
		return it.regs.Reg(rule.Reg), nil
	case frame.RuleExpression:
		v, _, err := op.ExecuteStackProgram(it.regs, rule.Expression, it.bi.Arch.PtrSize(), it.mem.ReadMemory)
		if err != nil {
			return nil, err
		}
		return it.readRegisterAt(regnum, uint64(v))
	case frame.RuleValExpression:
		v, _, err := op.ExecuteStackProgram(it.regs, rule.Expression, it.bi.Arch.PtrSize(), it.mem.ReadMemory)
		if err != nil {
			return nil, err
		}
		return op.DwarfRegisterFromUint64(uint64(v)), nil
	case frame.RuleArchitectural:
		return nil, errors.New("architectural frame rules are unsupported")
	case frame.RuleCFA:
		if it.regs.Reg(rule.Reg) == nil {
			return nil, nil
		}
		return op.DwarfRegisterFromUint64(uint64(int64(it.regs.Uint64Val(rule.Reg)) + rule.Offset)), nil
	case frame.RuleFramePointer:
		curReg := it.regs.Reg(rule.Reg)
		if curReg == nil {
			return nil, nil
		}
		if curReg.Uint64Val <= uint64(cfa) {
			return it.readRegisterAt(regnum, curReg.Uint64Val)
		}
		newReg := *curReg
		return &newReg, nil
	}
}

func (it *stackIterator) readRegisterAt(regnum uint64, addr uint64) (*op.DwarfRegister, error) {
	buf := make([]byte, it.bi.Arch.regSize(regnum))
	_, err := it.mem.ReadMemory(buf, addr)
	if err != nil {
		return nil, err
	}
	return op.DwarfRegisterFromBytes(buf), nil
}

func (it *stackIterator) loadG0SchedSP() {
	if it.g0_sched_sp_loaded {
		return
	}
	it.g0_sched_sp_loaded = true
	if it.g != nil {
		mvar, _ := it.g.variable.structMember("m")
		if mvar != nil {
			g0var, _ := mvar.structMember("g0")
			if g0var != nil {
				g0, _ := g0var.parseG()
				if g0 != nil {
					it.g0_sched_sp = g0.SP
				}
			}
		}
	}
}

// Defer represents one deferred call
type Defer struct {
	DwrapPC uint64 // PC of the deferred function or, in Go 1.17+ a wrapper to it
	DeferPC uint64 // PC address of instruction that added this defer
	SP      uint64 // Value of SP register when this function was deferred (this field gets adjusted when the stack is moved to match the new stack space)
	link    *Defer // Next deferred function
	argSz   int64  // Always 0 in Go >=1.17

	rangefunc []*Defer // See explanation in $GOROOT/src/runtime/panic.go, comment to function runtime.deferrangefunc (this is the equivalent of the rangefunc variable and head fields, combined)

	variable   *Variable
	Unreadable error
}

// readDefers decorates the frames with the function deferred at each stack frame.
func (g *G) readDefers(frames []Stackframe) {
	curdefer := g.Defer()
	i := 0

	// scan simultaneously frames and the curdefer linked list, assigning
	// defers to their associated frames.
	for {
		if curdefer == nil || i >= len(frames) {
			return
		}
		if curdefer.Unreadable != nil {
			// Current defer is unreadable, stick it into the first available frame
			// (so that it can be reported to the user) and exit
			frames[i].Defers = append(frames[i].Defers, curdefer)
			return
		}
		if frames[i].Err != nil {
			return
		}

		if frames[i].TopmostDefer == nil {
			frames[i].TopmostDefer = curdefer.topdefer()
		}

		if frames[i].SystemStack || frames[i].Inlined || curdefer.SP >= uint64(frames[i].Regs.CFA) {
			// frames[i].Regs.CFA is the value that SP had before the function of
			// frames[i] was called.
			// This means that when curdefer.SP == frames[i].Regs.CFA then curdefer
			// was added by the previous frame.
			//
			// curdefer.SP < frames[i].Regs.CFA means curdefer was added by a
			// function further down the stack.
			//
			// SystemStack frames live on a different physical stack and can't be
			// compared with deferred frames.
			i++
		} else {
			if len(curdefer.rangefunc) > 0 {
				frames[i].Defers = append(frames[i].Defers, curdefer.rangefunc...)
			} else {
				frames[i].Defers = append(frames[i].Defers, curdefer)
			}
			curdefer = curdefer.Next()
		}
	}
}

const maxRangeFuncDefers = 10

func (d *Defer) load(canrecur bool) {
	v := d.variable // +rtype _defer
	v.loadValue(LoadConfig{false, 1, 0, 0, -1, 0})
	if v.Unreadable != nil {
		d.Unreadable = v.Unreadable
		return
	}

	fnvar := v.fieldVariable("fn")
	if fnvar.Kind == reflect.Func {
		// In Go 1.18, fn is a func().
		d.DwrapPC = fnvar.Base
	} else if val := fnvar.maybeDereference(); val.Addr != 0 {
		// In Go <1.18, fn is a *funcval.
		fnvar = fnvar.loadFieldNamed("fn")
		if fnvar.Unreadable == nil {
			d.DwrapPC, _ = constant.Uint64Val(fnvar.Value)
		}
	}

	d.DeferPC, _ = constant.Uint64Val(v.fieldVariable("pc").Value) // +rtype uintptr
	d.SP, _ = constant.Uint64Val(v.fieldVariable("sp").Value)      // +rtype uintptr
	sizVar := v.fieldVariable("siz")                               // +rtype -opt int32
	if sizVar != nil {
		// In Go <1.18, siz stores the number of bytes of
		// defer arguments following the defer record. In Go
		// 1.18, the defer record doesn't store arguments, so
		// we leave this 0.
		d.argSz, _ = constant.Int64Val(sizVar.Value)
	}

	linkvar := v.fieldVariable("link").maybeDereference() // +rtype *_defer
	if linkvar.Addr != 0 {
		d.link = &Defer{variable: linkvar}
	}

	if canrecur {
		h := v
		for _, fieldname := range []string{"head", "u", "value"} {
			if h == nil {
				return
			}
			h = h.loadFieldNamed(fieldname)
		}
		if h != nil {
			h := h.newVariable("", h.Addr, pointerTo(linkvar.DwarfType, h.bi.Arch), h.mem).maybeDereference()
			if h.Addr != 0 {
				hd := &Defer{variable: h}
				for {
					hd.load(false)
					d.rangefunc = append(d.rangefunc, hd)
					if hd.link == nil {
						break
					}
					if len(d.rangefunc) > maxRangeFuncDefers {
						// We don't have a way to know for sure that we haven't gone completely off-road while loading this list so limit it to an arbitrary maximum size.
						break
					}
					hd = hd.link
				}
			}
		}
	}
}

// errSPDecreased is used when (*Defer).Next detects a corrupted linked
// list, specifically when after following a link pointer the value of SP
// decreases rather than increasing or staying the same (the defer list is a
// FIFO list, nodes further down the list have been added by function calls
// further down the call stack and therefore the SP should always increase).
var errSPDecreased = errors.New("corrupted defer list: SP decreased")

// Next returns the next defer in the linked list
func (d *Defer) Next() *Defer {
	if d.link == nil {
		return nil
	}
	d.link.load(true)
	if d.link.SP < d.SP {
		d.link.Unreadable = errSPDecreased
	}
	return d.link
}

func (d *Defer) topdefer() *Defer {
	if len(d.rangefunc) > 0 {
		return d.rangefunc[0]
	}
	return d
}

// EvalScope returns an EvalScope relative to the argument frame of this deferred call.
// The argument frame of a deferred call is stored in memory immediately
// after the deferred header.
func (d *Defer) EvalScope(t *Target, thread Thread) (*EvalScope, error) {
	scope, err := GoroutineScope(t, thread)
	if err != nil {
		return nil, fmt.Errorf("could not get scope: %v", err)
	}

	bi := thread.BinInfo()
	scope.PC = d.DwrapPC
	scope.File, scope.Line, scope.Fn = bi.PCToLine(d.DwrapPC)

	if scope.Fn == nil {
		return nil, fmt.Errorf("could not find function at %#x", d.DwrapPC)
	}

	// The arguments are stored immediately after the defer header struct, i.e.
	// addr+sizeof(_defer).

	if !bi.Arch.usesLR {
		// On architectures that don't have a link register CFA is always the address of the first
		// argument, that's what we use for the value of CFA.
		// For SP we use CFA minus the size of one pointer because that would be
		// the space occupied by pushing the return address on the stack during the
		// CALL.
		scope.Regs.CFA = (int64(d.variable.Addr) + d.variable.RealType.Common().ByteSize)
		scope.Regs.Reg(scope.Regs.SPRegNum).Uint64Val = uint64(scope.Regs.CFA - int64(bi.Arch.PtrSize()))
	} else {
		// On architectures that have a link register CFA and SP have the same
		// value but the address of the first argument is at CFA+ptrSize so we set
		// CFA to the start of the argument frame minus one pointer size.
		scope.Regs.CFA = int64(d.variable.Addr) + d.variable.RealType.Common().ByteSize - int64(bi.Arch.PtrSize())
		scope.Regs.Reg(scope.Regs.SPRegNum).Uint64Val = uint64(scope.Regs.CFA)
	}

	rdr := scope.Fn.cu.image.dwarfReader
	rdr.Seek(scope.Fn.offset)
	e, err := rdr.Next()
	if err != nil {
		return nil, fmt.Errorf("could not read DWARF function entry: %v", err)
	}
	scope.Regs.FrameBase, _, _, _ = bi.Location(e, dwarf.AttrFrameBase, scope.PC, scope.Regs, scope.Mem)
	scope.Mem = cacheMemory(scope.Mem, uint64(scope.Regs.CFA), int(d.argSz))

	return scope, nil
}

// DeferredFunc returns the deferred function, on Go 1.17 and later unwraps
// any defer wrapper.
func (d *Defer) DeferredFunc(p *Target) (file string, line int, fn *Function) {
	bi := p.BinInfo()
	fn = bi.PCToFunc(d.DwrapPC)
	fn = p.dwrapUnwrap(fn)
	if fn == nil {
		return "", 0, nil
	}
	file, line = bi.EntryLineForFunc(fn)
	return file, line, fn
}

func ruleString(rule *frame.DWRule, regnumToString func(uint64) string) string {
	switch rule.Rule {
	case frame.RuleUndefined:
		return "undefined"
	case frame.RuleSameVal:
		return "sameval"
	case frame.RuleOffset:
		return fmt.Sprintf("[cfa+%d]", rule.Offset)
	case frame.RuleValOffset:
		return fmt.Sprintf("cfa+%d", rule.Offset)
	case frame.RuleRegister:
		return fmt.Sprintf("R(%d)", rule.Reg)
	case frame.RuleExpression:
		w := &strings.Builder{}
		op.PrettyPrint(w, rule.Expression, regnumToString)
		return fmt.Sprintf("[expr(%s)]", w.String())
	case frame.RuleValExpression:
		w := &strings.Builder{}
		op.PrettyPrint(w, rule.Expression, regnumToString)
		return fmt.Sprintf("expr(%s)", w.String())
	case frame.RuleArchitectural:
		return "architectural"
	case frame.RuleCFA:
		return fmt.Sprintf("R(%d)+%d", rule.Reg, rule.Offset)
	case frame.RuleFramePointer:
		return fmt.Sprintf("[R(%d)] framepointer", rule.Reg)
	default:
		return fmt.Sprintf("unknown_rule(%d)", rule.Rule)
	}
}

// rangeFuncStackTrace, if the topmost frame of the stack is a the body of a
// range-over-func statement, returns a slice containing the stack of range
// bodies on the stack, interleaved with their return frames, the frame of
// the function containing them and finally the function that called it.
//
// For example, given:
//
//	func f() {
//		for _ := range iterator1 {
//			for _ := range iterator2 {
//				fmt.Println() // <- YOU ARE HERE
//			}
//		}
//	}
//
// It will return the following frames:
//
// 0. f-range2()
// 1. function that called f-range2
// 2. f-range1()
// 3. function that called f-range1
// 4. f()
// 5. function that called f()
//
// If the topmost frame of the stack is *not* the body closure of a
// range-over-func statement then nothing is returned.
func rangeFuncStackTrace(tgt *Target, g *G) ([]Stackframe, error) {
	if g == nil {
		return nil, nil
	}
	it, err := goroutineStackIterator(tgt, g, StacktraceSimple)
	if err != nil {
		return nil, err
	}
	frames := []Stackframe{}

	const (
		startStage = iota
		normalStage
		lastFrameStage
		doneStage
	)

	stage := startStage
	addRetFrame := false

	var rangeParent *Function
	nonMonotonicSP := false
	var closurePtr int64

	optimized := func(fn *Function) bool {
		return fn.cu.optimized&optimizedOptimized != 0
	}

	appendFrame := func(fr Stackframe) {
		frames = append(frames, fr)
		if fr.closurePtr != 0 {
			closurePtr = fr.closurePtr
		}
		addRetFrame = true
	}

	closurePtrOk := func(fr *Stackframe) bool {
		if fr.SystemStack {
			return false
		}
		if closurePtr == 0 && optimized(fr.Call.Fn) || frames[len(frames)-1].Inlined {
			return true
		}
		if closurePtr < 0 {
			// closure is stack allocated, check that it is on this frame
			return fr.contains(closurePtr)
		}
		// otherwise closurePtr is a heap allocated variable, so we need to check
		// all closure body variables in scope in this frame
		scope := FrameToScope(tgt, it.mem, it.g, 0, *fr)
		yields, _ := scope.simpleLocals(localsNoDeclLineCheck|localsOnlyRangeBodyClosures, "")
		for _, yield := range yields {
			if yield.Kind != reflect.Func {
				continue
			}
			addr := yield.funcvalAddr()
			if int64(addr) == closurePtr {
				return true
			}
		}
		return false
	}

	it.stacktraceFunc(func(fr Stackframe) bool {
		if len(frames) > 0 {
			prev := &frames[len(frames)-1]
			if fr.Regs.SP() < prev.Regs.SP() {
				nonMonotonicSP = true
				return false
			}
		}

		if addRetFrame {
			addRetFrame = false
			frames = append(frames, fr)
		}

		if fr.Call.Fn == nil {
			if stage == startStage {
				frames = nil
				addRetFrame = false
				stage = doneStage
				return false
			} else {
				return true
			}
		}

		switch stage {
		case startStage:
			appendFrame(fr)
			rangeParent = fr.Call.Fn.extra(tgt.BinInfo()).rangeParent
			stage = normalStage
			stop := false
			if rangeParent == nil {
				stop = true
			}
			if !optimized(fr.Call.Fn) && !fr.Inlined && closurePtr == 0 {
				stop = true
			}
			if stop {
				frames = nil
				addRetFrame = false
				stage = doneStage
				return false
			}
		case normalStage:
			if fr.Call.Fn.offset == rangeParent.offset && closurePtrOk(&fr) {
				frames = append(frames, fr)
				stage = lastFrameStage
			} else if fr.Call.Fn.extra(tgt.BinInfo()).rangeParent == rangeParent && closurePtrOk(&fr) {
				appendFrame(fr)
				if !optimized(fr.Call.Fn) && closurePtr == 0 {
					frames = nil
					addRetFrame = false
					stage = doneStage
					return false
				}
			} else if frames[len(frames)-1].Inlined && !fr.Inlined && closurePtr == 0 {
				frames = nil
				addRetFrame = false
				stage = doneStage
				return false
			}
		case lastFrameStage:
			frames = append(frames, fr)
			stage = doneStage
			return false
		case doneStage:
			return false
		}
		return true
	})
	if it.Err() != nil {
		return nil, it.Err()
	}
	if nonMonotonicSP {
		return nil, errors.New("corrupted stack (SP not monotonically decreasing)")
	}
	if stage != doneStage {
		return nil, errors.New("could not find range-over-func closure parent on the stack")
	}
	if len(frames)%2 != 0 {
		return nil, errors.New("incomplete range-over-func stacktrace")
	}
	g.readDefers(frames)
	return frames, nil
}
