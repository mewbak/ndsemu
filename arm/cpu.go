package arm

import (
	"ndsemu/emu"
	"ndsemu/emu/debugger"
	"ndsemu/emu/jit"
	log "ndsemu/emu/logger"
)

type Arch int
type Line int

const (
	// NOTE: the order is important. Code can do things like "if arch <) ARMv4"
	// to mean "ARMv4 and earlier"
	ARMv4 Arch = 4
	ARMv5 Arch = 5
)

const (
	LineFiq Line = 1 << iota
	LineIrq
	LineHalt
)

type Cpu struct {
	Regs  [16]reg
	Cpsr  regCpsr
	Clock int64

	UsrBank  [2]reg
	FiqBank  [2]reg
	SvcBank  [2]reg
	AbtBank  [2]reg
	IrqBank  [2]reg
	UndBank  [2]reg
	SpsrBank [5]reg

	UsrBank2 [5]reg
	FiqBank2 [5]reg

	arch  Arch
	bus   emu.Bus
	pc    reg
	cp15  *Cp15
	cops  [16]Coprocessor
	lines Line
	jit   *jit.Jit

	// Optional HLE implementation of SWIs
	swiHle [256]func(cpu *Cpu) int64

	// Store the previous PC, used for debugging (eg: jumping into nowhere)
	prevpc reg

	// Number of cycles consumed when accessing the external bus
	memCycles    int64
	targetCycles int64
	tightExit    bool

	// manual tracing support
	DebugTrace int
	dbg        debugger.CpuDebugger
}

func NewCpu(arch Arch, bus emu.Bus, dojit bool) *Cpu {
	cpu := &Cpu{bus: bus, arch: arch}
	cpu.Cpsr._mode = 0x13 // mode supervisor
	cpu.memCycles = int64(bus.WaitStates() + 1)
	if dojit {
		cpu.jit = jit.NewJit(cpu, &jit.Config{
			PcAlignmentShift: 2,
			MaxBlockSize:     1024,
		})
	}
	return cpu
}

func (cpu *Cpu) Jit() *jit.Jit {
	return cpu.jit
}

func (cpu *Cpu) SetPC(addr uint32) {
	cpu.Regs[15] = reg(addr)
	cpu.pc = cpu.Regs[15]
}

func (cpu *Cpu) RegSpsr() *reg {
	mode := cpu.Cpsr.GetMode()
	switch mode {
	case CpuModeUser, CpuModeSystem:
		cpu.breakpoint("access to spsr forbidden in non-privileged mode: %v", mode)
		return &cpu.SpsrBank[0] // unreachable, unless debugger
	case CpuModeFiq:
		return &cpu.SpsrBank[0]
	case CpuModeSupervisor:
		return &cpu.SpsrBank[1]
	case CpuModeAbort:
		return &cpu.SpsrBank[2]
	case CpuModeIrq:
		return &cpu.SpsrBank[3]
	case CpuModeUndefined:
		return &cpu.SpsrBank[4]
	default:
		cpu.breakpoint("unsupported mode in RegSpsr(): %v", mode)
		return &cpu.SpsrBank[0] // unreachable, unless debugger
	}
}

func (cpu *Cpu) MapCoprocessor(copnum int, cop Coprocessor) {
	cpu.cops[copnum] = cop
}

func (cpu *Cpu) EnableCp15() *Cp15 {
	cpu.cp15 = newCp15(cpu)
	cpu.cops[15] = cpu.cp15
	return cpu.cp15
}

type Exception int

const (
	ExceptionReset           Exception = 0
	ExceptionUndefined       Exception = 1
	ExceptionSwi             Exception = 2
	ExceptionPrefetchAbort   Exception = 3
	ExceptionDataAbort       Exception = 4
	ExceptionAddressOverflow Exception = 5
	ExceptionIrq             Exception = 6
	ExceptionFiq             Exception = 7
)

// CPU mode to enter when the exception is raised
var excMode = [8]CpuMode{
	CpuModeSupervisor,
	CpuModeUndefined,
	CpuModeSupervisor,
	CpuModeAbort,
	CpuModeAbort,
	CpuModeSupervisor,
	CpuModeIrq,
	CpuModeFiq,
}

var excPcOffsetArm = [8]uint32{
	0, 4, 0, 4, 8, 4, 4, 4,
}
var excPcOffsetThumb = [8]uint32{
	0, 2, 0, 4, 6, 2, 4, 4,
}

func (cpu *Cpu) Exception(exc Exception) {
	newmode := excMode[exc]

	pc := cpu.pc
	if cpu.Cpsr.T() {
		pc += reg(excPcOffsetThumb[exc])
	} else {
		pc += reg(excPcOffsetArm[exc])
	}

	// If the exception is a SWI, check if there's a HLE emulation
	// installed for this. If so, run it and then immediately exit,
	// without triggering a real exception in the ARM core.
	if exc == ExceptionSwi {
		num := cpu.Read16(uint32(pc-2)) & 0xFF
		if hle := cpu.swiHle[num]; hle != nil {
			// cpu.breakpoint("hle")
			log.ModCpu.InfoZ("SWI - HLE emulation").
				Uint16("num", num).
				Uint16("exc", uint16(exc)).
				End()
			delay := hle(cpu)
			cpu.Clock += delay + 3
			return
		}
		log.ModCpu.InfoZ("SWI").
			Hex16("num", num).
			End()
	} else {
		log.ModCpu.InfoZ("exception").
			Int("exc", int(exc)).
			Hex32("LR", uint32(pc)).
			Int("arch", int(cpu.arch)).
			End()
	}

	oldcpsr := cpu.Cpsr.Uint32()

	// Adjust CPSR for interrupt mode
	cpu.Cpsr.SetT(false, cpu)
	cpu.Cpsr.SetMode(newmode, cpu)
	cpu.Cpsr.SetI(true, cpu)
	if exc == ExceptionReset || exc == ExceptionFiq {
		cpu.Cpsr.SetF(true, cpu)
	}

	// Save old CPSR into SPSR, and old PC into R14.
	// Do this only after mode change, so that we use the correct bank.
	*cpu.RegSpsr() = reg(oldcpsr)
	cpu.Regs[14] = pc
	if cpu.cp15 != nil {
		cpu.Regs[15] = reg(cpu.cp15.ExceptionVector())
	} else {
		cpu.Regs[15] = 0x00000000
	}

	cpu.Regs[15] += reg(exc * 4)
	cpu.branch(cpu.Regs[15], BranchInterrupt)
	cpu.Clock += 3
}

// Install a high-level emulation function for a specific SWI call.
// This function can be used to simulate specific SWI calls (usually
// implemented by the BIOS/OS) replacing them with code within code
// written in the emulator itself. This can be useful for several
// scenarios:
//   * To make the emulator work without the original BIOS images; this
//     can be useful for copyright concerns. In this case, all used SWIs
//     should be replaced by HLE functions.
//   * To speed up the emulation, by replacing frequently used SWI calls
//     with an equivalent code that is faster in emulated execution.
//
// The installed HLE function takes no parameter; parameters to the call
// are probably passed in registers, so the the function probably needs to
// access the cpu.Regs array anyway.
// The return value is the number of cycles that we should advance the CPU
// clock of; it should correspond to a value closer to the time the real
// function would have taken, were it fully interpreted.
func (cpu *Cpu) SetSwiHle(swi uint8, hle func(cpu *Cpu) int64) {
	cpu.swiHle[swi] = hle
}

// Set the status of the external (virtual) lines. This is modeled
// to resemble the physical lines of the CPU core, but without the
// need of full fidelity to high/low signals or clocking.
//
// For virtual lines, "true" means "activate the function", while
// "false" means "disable the function" (irrespecitve of the physical
// high/low signal required by the core).
func (cpu *Cpu) SetLine(line Line, val bool) {
	if val {
		// Any activation of new lines must be checked immediately,
		// so we need to exit from the tight loop where the lines are ignored.
		if cpu.lines^line != 0 {
			cpu.tightExit = true
		}
		cpu.lines |= line
		// Asserting IRQ/FIQ line immediately releases the HALT
		// status (even if interrupts are masked in CPSR flags)
		if line&(LineFiq|LineIrq) != 0 {
			cpu.lines &^= LineHalt
		}
	} else {
		cpu.lines &^= line
	}
}

func (cpu *Cpu) Reset() {
	cpu.pc = 0
	cpu.prevpc = 0
	cpu.Clock = 0
	cpu.Exception(ExceptionReset)
}
