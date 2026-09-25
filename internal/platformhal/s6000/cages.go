package s6000

import (
	"fmt"

	"github.com/salvaged-silicon/nosaic-switch/internal/platformhal"
)

// The QSFP control vectors are 32 bits, one per cage: bits 0-15 in the slave
// CPLD, 16-31 in the master, low byte first (dell_s6000_platform.c).
const (
	regModSelLo = 0x00 // slave; master 0x0a. Active low; also the i2c select
	regLPModeLo = 0x02 // slave; master 0x0c
	regModPrsLo = 0x04 // slave; master 0x0e. Active low
	regResetLo  = 0x06 // slave; master 0x10. Active low

	regModSelHi = 0x0a
	regLPModeHi = 0x0c
	regModPrsHi = 0x0e
	regResetHi  = 0x10

	cageCount = 32
)

// setCageVector writes one 32-bit control vector across both CPLDs.
func (h *HAL) setCageVector(lo, hi int, v uint32) error {
	return h.t.on(chSystem, func(b bus) error {
		for i, w := range []struct {
			addr, reg int
		}{{cpldSlave, lo}, {cpldSlave, lo + 1}, {cpldMaster, hi}, {cpldMaster, hi + 1}} {
			if err := b.WriteReg(w.addr, w.reg, byte(v>>(8*i))); err != nil {
				return err
			}
		}
		return nil
	})
}

func (h *HAL) cageVector(lo, hi int) (uint32, error) {
	var v uint32
	err := h.t.on(chSystem, func(b bus) error {
		v = 0
		for i, r := range []struct {
			addr, reg int
		}{{cpldSlave, lo}, {cpldSlave, lo + 1}, {cpldMaster, hi}, {cpldMaster, hi + 1}} {
			x, err := b.ReadReg(r.addr, r.reg)
			if err != nil {
				return err
			}
			v |= uint32(x) << (8 * i)
		}
		return nil
	})
	return v, err
}

// CagesPresent reports which cages hold a module.
func (h *HAL) CagesPresent() ([cageCount]bool, error) {
	var out [cageCount]bool
	v, err := h.cageVector(regModPrsLo, regModPrsHi)
	if err != nil {
		return out, err
	}
	for i := range out {
		out[i] = v&(1<<i) == 0
	}
	return out, nil
}

// CageCount is platformhal.Optics.
func (h *HAL) CageCount() int { return cageCount }

// ReadModuleBytes is platformhal.Optics: select the cage through its CPLD,
// switch the GPIO mux to that CPLD's group, and read the module.
//
// Both steps under one hold of the tree lock, because the CPLD select is on
// channel 0 and the module on channel 2 or 3: releasing between them would let
// another reader move the mux or reselect a different cage.
func (h *HAL) ReadModuleBytes(cage, addr, page, offset, n int) ([]byte, error) {
	if cage < 1 || cage > cageCount {
		return nil, fmt.Errorf("cage %d: this board has cages 1-%d", cage, cageCount)
	}
	if addr != qsfpEEPROM {
		return nil, fmt.Errorf("%w: QSFP modules answer only at %#02x", platformhal.ErrUnsupported, qsfpEEPROM)
	}
	if offset < 0 || n <= 0 || offset+n > 256 {
		return nil, fmt.Errorf("offset %d length %d is outside the module's 256 bytes", offset, n)
	}
	i := cage - 1
	cpld, reg, ch := cpldSlave, regModSelLo, chQSFPLo
	if i >= 16 {
		cpld, reg, ch = cpldMaster, regModSelHi, chQSFPHi
		i -= 16
	}
	mask := ^uint16(1 << i)

	var out []byte
	err := h.t.seq(
		step{chSystem, func(b bus) error {
			if err := b.WriteReg(cpld, reg, byte(mask)); err != nil {
				return err
			}
			return b.WriteReg(cpld, reg+1, byte(mask>>8))
		}},
		step{ch, func(b bus) error {
			if page >= 0 {
				// SFF-8636: the page select is byte 127.
				if err := b.WriteReg(qsfpEEPROM, 127, byte(page)); err != nil {
					return err
				}
			}
			var err error
			out, err = b.ReadAt(qsfpEEPROM, offset, n)
			return err
		}},
	)
	if err != nil {
		return nil, fmt.Errorf("cage %d: %w", cage, err)
	}
	return out, nil
}
