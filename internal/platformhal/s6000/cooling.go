package s6000

import (
	"errors"
	"fmt"

	"github.com/salvaged-silicon/nosaic-switch/internal/platformhal"
)

// fanMaxRPM is 100%: SONiC's MAX_S6000_FAN_SPEED, and the top level of
// Dell's own fancontrol.sh.
const fanMaxRPM = 19000

// max6620 registers (max6620.c).
const (
	m6620Config = 0x00
	m6620Conf0  = 0x02 // per fan +i
	m6620Dyn0   = 0x06 // per fan +i
	m6620Tach0  = 0x10 // per fan +2i, two bytes
	m6620Tar0   = 0x20 // per fan +2i, two bytes
)

// The six fans, in the order Dell's set-fan-speed script numbers them, with
// the tray each belongs to.
//
// ⚠ THE TRAY PAIRING IS NOT SETTLED. Dell's own files disagree: fancontrol.sh
// pairs 0x29 fans 1-2 as tray 1, 0x29 fans 3-4 as tray 2 and 0x2a fans 1-2 as
// tray 3; SONiC's fan.py reverses trays 1 and 3; set-fan-speed's comment
// pairs "fan1-fan4 fan2-fan5 fan3-fan6". This follows fancontrol.sh, the one
// that actually drove the fans, and each fan's Raw says which controller and
// channel it is so a unit can settle it by pulling a tray.
var fans = []struct {
	ctl, ch, tray int
}{
	{fanCtlA, 0, 1}, {fanCtlA, 1, 1},
	{fanCtlA, 2, 2}, {fanCtlA, 3, 2},
	{fanCtlB, 0, 3}, {fanCtlB, 1, 3},
}

// FanCount is platformhal.Cooling.
func (h *HAL) FanCount() int { return len(fans) }

// FanFloorPercent is platformhal.Cooling.
func (h *HAL) FanFloorPercent() int { return h.d.floor() }

func (h *HAL) trays() ([3]bool, error) {
	var r8, r9 byte
	err := h.t.on(chSystem, func(b bus) error {
		var err error
		if r8, err = b.ReadReg(cpldMaster, regFanTrays); err != nil {
			return err
		}
		r9, err = b.ReadReg(cpldMaster, regFanTray3)
		return err
	})
	return fanTrays(r8, r9), err
}

// Fans reads each fan's speed and commanded target.
func (h *HAL) Fans() ([]platformhal.Fan, error) {
	trays, err := h.trays()
	if err != nil {
		return nil, fmt.Errorf("fan tray presence: %w", err)
	}
	out := make([]platformhal.Fan, len(fans))
	err = h.t.on(chFans, func(b bus) error {
		for i, f := range fans {
			fan := platformhal.Fan{Index: i + 1, Present: trays[f.tray-1],
				Raw: fmt.Sprintf("max6620 %#02x ch%d, tray %d", f.ctl, f.ch, f.tray)}
			dyn, err := b.ReadReg(f.ctl, m6620Dyn0+f.ch)
			if err != nil {
				return err
			}
			div := max6620Div(dyn)
			th, err := b.ReadReg(f.ctl, m6620Tach0+2*f.ch)
			if err != nil {
				return err
			}
			tl, err := b.ReadReg(f.ctl, m6620Tach0+2*f.ch+1)
			if err != nil {
				return err
			}
			fan.RPM = max6620RPM(div, max6620Count(th, tl))
			gh, _ := b.ReadReg(f.ctl, m6620Tar0+2*f.ch)
			gl, _ := b.ReadReg(f.ctl, m6620Tar0+2*f.ch+1)
			if target := max6620RPM(div, max6620Count(gh, gl)); target > 0 {
				fan.Percent = (target*100 + fanMaxRPM/2) / fanMaxRPM
			}
			out[i] = fan
		}
		return nil
	})
	return out, err
}

// SetFanPercent sets every fan's RPM target, never below the floor.
//
// The max6620s run closed-loop on RPM, as SONiC runs them: this writes a
// target tachometer count and the chip holds the speed. The first call puts
// both chips in RPM mode exactly as the Linux driver's probe does -- config
// bit 4, each fan's config |= 0xa8, dynamics 0x30 (divider 2, 0.125 s rate).
func (h *HAL) SetFanPercent(percent int) (int, error) {
	if percent < h.d.floor() {
		percent = h.d.floor()
	}
	if percent > 100 {
		percent = 100
	}
	rpm := fanMaxRPM * percent / 100
	refused := 0
	var errs []error
	err := h.t.on(chFans, func(b bus) error {
		if !h.fanUp {
			if err := initMax6620(b, fanCtlA, 4); err != nil {
				return fmt.Errorf("max6620 %#02x: %w", fanCtlA, err)
			}
			if err := initMax6620(b, fanCtlB, 2); err != nil {
				return fmt.Errorf("max6620 %#02x: %w", fanCtlB, err)
			}
			h.fanUp = true
		}
		refused, errs = 0, nil
		for i, f := range fans {
			dyn, err := b.ReadReg(f.ctl, m6620Dyn0+f.ch)
			if err == nil {
				hi, lo := max6620Target(max6620Div(dyn), rpm)
				if err = b.WriteReg(f.ctl, m6620Tar0+2*f.ch, hi); err == nil {
					err = b.WriteReg(f.ctl, m6620Tar0+2*f.ch+1, lo)
				}
			}
			if err != nil {
				refused++
				errs = append(errs, fmt.Errorf("fan %d: %w", i+1, err))
			}
		}
		return nil
	})
	if err != nil {
		return len(fans), err
	}
	return refused, errors.Join(errs...)
}

func initMax6620(b bus, ctl, n int) error {
	c, err := b.ReadReg(ctl, m6620Config)
	if err != nil {
		return err
	}
	if err := b.WriteReg(ctl, m6620Config, c|0x10); err != nil {
		return err
	}
	for i := 0; i < n; i++ {
		fc, err := b.ReadReg(ctl, m6620Conf0+i)
		if err != nil {
			return err
		}
		if err := b.WriteReg(ctl, m6620Conf0+i, fc|0xa8); err != nil {
			return err
		}
		if err := b.WriteReg(ctl, m6620Dyn0+i, 0x30); err != nil {
			return err
		}
	}
	return nil
}

// LED codes in the master CPLD (dell_s6000_platform.c).
const (
	sysBlinkGreen = 0
	sysGreen      = 1
	sysYellow     = 2

	trayGreen  = 1
	trayYellow = 2

	frontYellow = 1
	frontGreen  = 2
)

// HealthLamps is thermal.Lamps: the system LED says whether the box is
// within its thermal band, the tray LEDs and the front fan LED whether the
// fans are all turning.
func (h *HAL) HealthLamps(fl []platformhal.Fan, hottestC, maxC int, fanErr error) error {
	trayOK := [3]bool{true, true, true}
	for i, f := range fl {
		if i < len(fans) && (!f.Present || f.RPM == 0) {
			trayOK[fans[i].tray-1] = false
		}
	}
	allOK := fanErr == nil && trayOK[0] && trayOK[1] && trayOK[2]
	sys := sysGreen
	if !allOK || hottestC >= maxC {
		sys = sysYellow
	}
	return h.t.on(chSystem, func(b bus) error {
		leds, err := b.ReadReg(cpldMaster, regLEDs)
		if err != nil {
			return err
		}
		if err := b.WriteReg(cpldMaster, regLEDs, leds&0x9f|byte(sys)<<5); err != nil {
			return err
		}
		r8, err := b.ReadReg(cpldMaster, regFanTrays)
		if err != nil {
			return err
		}
		r8 &= 0xc0
		for t, ok := range trayOK {
			c := trayGreen
			if !ok {
				c = trayYellow
			}
			r8 |= byte(c) << (2 * t)
		}
		if err := b.WriteReg(cpldMaster, regFanTrays, r8); err != nil {
			return err
		}
		r9, err := b.ReadReg(cpldMaster, regFanTray3)
		if err != nil {
			return err
		}
		front := frontGreen
		if !allOK {
			front = frontYellow
		}
		return b.WriteReg(cpldMaster, regFanTray3, r9&0xe7|byte(front)<<3)
	})
}

// LampsUnmanaged returns the system LED to blinking green, the CPLD's own
// "not yet up" state, so a stopped thermal loop does not leave a stale
// steady green claiming everything is fine.
func (h *HAL) LampsUnmanaged() error {
	return h.t.on(chSystem, func(b bus) error {
		leds, err := b.ReadReg(cpldMaster, regLEDs)
		if err != nil {
			return err
		}
		return b.WriteReg(cpldMaster, regLEDs, leds&0x9f|byte(sysBlinkGreen)<<5)
	})
}
