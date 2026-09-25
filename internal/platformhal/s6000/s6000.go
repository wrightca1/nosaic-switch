package s6000

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/salvaged-silicon/nosaic-switch/internal/platformhal"
)

// Master CPLD registers used outside the cages (dell_s6000_platform.c).
const (
	regMasterVersion = 0x01 // [3:0]
	regPSU           = 0x03
	regLEDs          = 0x07 // [6:5] system, [4:3] locator, [2:1] power, [0] master
	regFanTrays      = 0x08 // [7:6] trays 1-2 present; [5:4],[3:2],[1:0] tray LEDs
	regFanTray3      = 0x09 // [0] tray 3 present; [4:3] front fan LED

	regSystemVersion = 0x00 // system CPLD [3:0]
	regPowerReset    = 0x01 // system CPLD: write 0xfd to power-cycle the board
	powerCycle       = 0xfd

	regSlaveVersion = 0x0a // slave CPLD [3:0]
)

// HAL is the S6000-ON's platform hardware.
type HAL struct {
	d     *Data
	t     *tree
	psu   bus
	fanUp bool // the max6620s have been put in RPM mode by this process
}

func init() {
	platformhal.Register("dell-s6000", func(cfg platformhal.Config) (platformhal.HAL, error) {
		return Open(cfg)
	})
}

// Open finds the two buses and the GPIO chip, and checks the system CPLD
// answers where the board says it is.
func Open(cfg platformhal.Config) (*HAL, error) {
	d, _ := cfg.BoardData.(*Data)
	if d == nil {
		return nil, fmt.Errorf("%w: this board declares no platform_hal.dell_s6000 block",
			platformhal.ErrUnsupported)
	}
	if err := d.Validate(); err != nil {
		return nil, fmt.Errorf("platform_hal.dell_s6000: %w", err)
	}
	parent, err := openBus(d.MuxParent)
	if err != nil {
		return nil, err
	}
	g, err := openGPIO(d.GPIOChip)
	if err != nil {
		parent.Close()
		return nil, err
	}
	h := &HAL{d: d, t: &tree{b: parent, gpio: g}}

	// ⚠ WHICH iSMT IS WHICH IS A STATEMENT IN board.yml, AND THIS IS WHERE
	// IT IS CHECKED. The two functions are indistinguishable by name. A
	// swapped pair would otherwise drive the GPIO mux over the PSU bus and
	// read whatever answers there as a CPLD.
	if _, err := h.cpldVersions(); err != nil {
		parent.Close()
		return nil, fmt.Errorf("no system CPLD at %#02x behind mux channel %d on %s: %w. "+
			"If this is a new unit, mux_parent and psu_bus in board.yml may be the other way round",
			cpldSystem, chSystem, d.MuxParent, err)
	}
	if h.psu, err = openBus(d.PSUBus); err != nil {
		parent.Close()
		return nil, err
	}
	return h, nil
}

// Close releases the buses.
func (h *HAL) Close() error {
	err := h.t.b.Close()
	if h.psu != nil {
		err = errors.Join(err, h.psu.Close())
	}
	return err
}

// cpldVersions reads the three CPLDs' version nibbles.
func (h *HAL) cpldVersions() ([3]int, error) {
	var v [3]int
	err := h.t.on(chSystem, func(b bus) error {
		for i, r := range []struct{ addr, reg int }{
			{cpldSystem, regSystemVersion}, {cpldMaster, regMasterVersion}, {cpldSlave, regSlaveVersion},
		} {
			x, err := b.ReadReg(r.addr, r.reg)
			if err != nil {
				return err
			}
			v[i] = int(x & 0x0f)
		}
		return nil
	})
	return v, err
}

// CPLDVersions is for `nosaic platform status`: system, master, slave.
func (h *HAL) CPLDVersions() (string, error) {
	v, err := h.cpldVersions()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("system %d, master %d, slave %d", v[0], v[1], v[2]), nil
}

// Board reads the Dell-format board EEPROM.
func (h *HAL) Board() (platformhal.Identity, error) {
	var e []byte
	err := h.t.on(chSystem, func(b bus) error {
		var err error
		e, err = b.ReadAt(sysEEPROM, 0, 128)
		return err
	})
	if err != nil {
		return platformhal.Identity{}, err
	}
	pn, serial, rev, ppid, mac, err := dellIdentity(e)
	if err != nil {
		return platformhal.Identity{}, err
	}
	model := "Dell S6000-ON"
	if pn != "" {
		model += " " + pn
	}
	return platformhal.Identity{Model: model, Serial: serial, Revision: rev, SID: ppid, MAC: mac}, nil
}

// Temperatures reads every sensor it can: three tmp75s on the fan channel,
// the emc1403 and jc42 on the system channel. A sensor that does not answer
// is left out rather than reported as zero; only no answer at all is an error.
func (h *HAL) Temperatures() (map[string]int, error) {
	out := map[string]int{}
	var errs []error
	h.t.on(chFans, func(b bus) error {
		for _, s := range tmp75s {
			if v, err := b.ReadWord(s.addr, 0x00); err == nil {
				out[s.name] = tmp75MilliC(v)
			} else {
				errs = append(errs, fmt.Errorf("%s: %w", s.name, err))
			}
		}
		return nil
	})
	h.t.on(chSystem, func(b bus) error {
		for _, c := range emc1403Channels {
			hi, err := b.ReadReg(emc1403Addr, c.hi)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", c.name, err))
				continue
			}
			lo, _ := b.ReadReg(emc1403Addr, c.lo)
			out[c.name] = emc1403MilliC(hi, lo)
		}
		if v, err := b.ReadWord(jc42Addr, 0x05); err == nil {
			out["DIMM"] = jc42MilliC(v)
		} else {
			errs = append(errs, fmt.Errorf("DIMM: %w", err))
		}
		return nil
	})
	if len(out) == 0 {
		return nil, fmt.Errorf("no temperature sensor answered: %w", errors.Join(errs...))
	}
	return out, nil
}

// PSUPresent reads the master CPLD's supply status.
func (h *HAL) PSUPresent() (map[string]bool, error) {
	r, err := h.readMaster(regPSU)
	if err != nil {
		return nil, err
	}
	p1, _ := psuNibble(r, 1)
	p2, _ := psuNibble(r, 2)
	return map[string]bool{"PSU1": p1, "PSU2": p2}, nil
}

// PSUHealthy is present-and-not-failed, per supply.
func (h *HAL) PSUHealthy() (map[string]bool, error) {
	r, err := h.readMaster(regPSU)
	if err != nil {
		return nil, err
	}
	_, ok1 := psuNibble(r, 1)
	_, ok2 := psuNibble(r, 2)
	return map[string]bool{"PSU1": ok1, "PSU2": ok2}, nil
}

func (h *HAL) readMaster(reg int) (byte, error) {
	var v byte
	err := h.t.on(chSystem, func(b bus) error {
		var err error
		v, err = b.ReadReg(cpldMaster, reg)
		return err
	})
	return v, err
}

// ResetState: the S6000's CPLDs expose no switch-chip reset line to read.
func (h *HAL) ResetState(platformhal.Reset) (bool, error) {
	return false, platformhal.ErrUnsupported
}

// Watchdog: none is documented for this board.
func (h *HAL) Watchdog() (platformhal.Watchdog, error) {
	return nil, platformhal.ErrUnsupported
}

// ReleaseSwitchChip does what Dell's init does before the ASIC's ports are
// used: every cage out of low-power mode, then a one-second reset pulse on
// all of them. The ASIC itself needs no release on this board -- it is on
// PCIe from power-on.
func (h *HAL) ReleaseSwitchChip(ctx context.Context) error {
	if err := h.setCageVector(regLPModeLo, regLPModeHi, 0x00000000); err != nil {
		return fmt.Errorf("taking cages out of low power: %w", err)
	}
	if err := h.setCageVector(regResetLo, regResetHi, 0x00000000); err != nil {
		return fmt.Errorf("asserting cage reset: %w", err)
	}
	select {
	case <-time.After(time.Second):
	case <-ctx.Done():
	}
	// Released even if the context ended: cages left in reset are dark.
	if err := h.setCageVector(regResetLo, regResetHi, 0xffffffff); err != nil {
		return fmt.Errorf("releasing cage reset: %w", err)
	}
	return ctx.Err()
}

// PowerCycle cuts power to the whole board through the system CPLD.
//
// ⚠ THIS IS HOW THIS BOARD REBOOTS. Dell replaces reboot with this write in
// both ONIE and SONiC: "triggers a hard system reboot, required by ASIC to
// operate correctly". A plain CPU reset leaves the Trident II as it was.
// Nothing after this call runs; sync before calling it.
func (h *HAL) PowerCycle() error {
	return h.t.on(chSystem, func(b bus) error {
		return b.WriteReg(cpldSystem, regPowerReset, powerCycle)
	})
}
