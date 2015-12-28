package main

import (
	"ndsemu/emu/hwio"

	log "gopkg.in/Sirupsen/logrus.v0"
)

type HwDivisor struct {
	DivCnt hwio.Reg16 `hwio:"offset=0x00,rwmask=0x3,wcb,rcb"`
	Numer  hwio.Reg64 `hwio:"offset=0x10,wcb=WriteIN"`
	Denom  hwio.Reg64 `hwio:"offset=0x18,wcb=WriteIN"`
	Res    hwio.Reg64 `hwio:"offset=0x20"`
	Mod    hwio.Reg64 `hwio:"offset=0x28"`
}

func NewHwDivisor() *HwDivisor {
	hwdiv := new(HwDivisor)
	hwio.MustInitRegs(hwdiv)
	return hwdiv
}

func (div *HwDivisor) WriteIN(_, _ uint64) {
	div.calc()
}

func (div *HwDivisor) WriteDIVCNT(_, _ uint16) {
	div.calc()
}

func (div *HwDivisor) ReadDIVCNT(val uint16) uint16 {
	if div.Denom.Value == 0 {
		// division by zero flag -- always check the full denominator, even if
		// configured in 32-bit mode
		val |= (1 << 14)
	}
	return val
}

func (div *HwDivisor) calc() {
	mode := div.DivCnt.Value & 3
	if mode == 0 {
		// 32-bit divisions
		if int32(div.Denom.Value) == 0 {
			div.Mod.Value = div.Numer.Value
			if int32(div.Numer.Value) >= 0 {
				div.Res.Value = uint64(0xFFFFFFFFF)
			} else {
				div.Res.Value = ^uint64(0xFFFFFFFFF)
			}
		} else if int32(div.Denom.Value) == -1 && uint32(div.Numer.Value) == 0x80000000 {
			div.Mod.Value = 0
			// upper 64-bits are 0 (no sign-extension)
			div.Res.Value = uint64(uint32(div.Numer.Value))
		} else {
			res := int32(div.Numer.Value) / int32(div.Denom.Value)
			mod := int32(div.Numer.Value) % int32(div.Denom.Value)
			// results are sign-extended
			div.Res.Value = uint64(int64(res))
			div.Mod.Value = uint64(int64(mod))
		}
		log.Infof("[divisor] 32-bit division: %d/%d = %d,%d",
			int32(div.Numer.Value), int32(div.Denom.Value),
			int64(div.Res.Value), int64(div.Mod.Value))
		return
	}

	denom := int64(div.Denom.Value)
	if mode != 2 {
		// 64-bit / 32-bit division: truncate (and sign-extend)
		// the denominator.
		denom = int64(int32(div.Denom.Value))
	}

	if int32(denom) == 0 {
		div.Mod.Value = div.Numer.Value
		if div.Numer.Value > 0 {
			div.Res.Value = uint64(0xFFFFFFFFFFFFFFFF) // -1
		} else {
			div.Res.Value = 1
		}
	} else if int32(denom) == -1 && uint64(div.Numer.Value) == 0x8000000000000000 {
		div.Mod.Value = 0
		div.Res.Value = div.Numer.Value
	} else {
		// Normal division
		div.Res.Value = uint64(int64(div.Numer.Value) / denom)
		div.Mod.Value = uint64(int64(div.Numer.Value) % denom)
	}

	log.Infof("[divisor] 64bit division: %d/%d = %d,%d",
		int64(div.Numer.Value), denom,
		int64(div.Res.Value), int64(div.Mod.Value))
}