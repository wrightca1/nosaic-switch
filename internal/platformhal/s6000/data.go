// Package s6000 is the platform HAL for the Dell S6000-ON: its CPLDs, fans,
// power supplies, temperature sensors and transceivers.
//
// # Everything is behind a mux the kernel does not own
//
// The S1220 has three SMBus controllers. One iSMT function carries the power
// supplies directly; the other is unused. The legacy SCH SMBus on the LPC
// bridge reaches everything else through a 74CBTLV3253 -- a dual 1:4 analog
// switch whose channel is chosen by two SoC GPIO lines -- and two of that
// switch's four channels fan out again, sixteen QSFP cages each, through a
// select register in a CPLD. (Read off a running unit: SONiC's i2c-2 is
// 00:1f.0/isch_smbus.N/i2c-2, i2c-1 is 00:13.1.)
//
// Dell's SONiC driver builds all of that in the kernel: an i2c-mux-gpio
// platform device, two register-driven mux adapters, and kernel hwmon drivers
// bound behind them. NOSaic has no out-of-tree module for it and no way to
// declare a GPIO mux on x86, which has no device tree. So this HAL drives the
// tree from userspace: it sets the GPIO lines through the gpio character
// device, writes the CPLD select registers itself, and reads every part
// directly through /dev/i2c-N -- a tmp75, an emc1403, a jc42, two max6620s,
// the CPLDs and the QSFP EEPROMs -- decoding each the way its Linux driver
// does.
//
// The consequence to keep in mind: no kernel driver may be bound to anything
// behind the mux. A kernel driver reading while this HAL has the mux on
// another channel would read the wrong device and report it as its own.
//
// Everything here is derived from Dell's GPL platform driver and scripts in
// sonic-buildimage (platform/broadcom/sonic-platform-modules-dell/s6000) and
// from the Linux drivers for each part. None of it has run on an S6000 yet.
package s6000

import (
	"fmt"
	"regexp"
)

// Data is this board's platform_hal.dell_s6000 block in board.yml.
type Data struct {
	// MuxParent is the PCI function whose SMBus the 74CBTLV3253 hangs
	// off: "0000:00:1f.0", the LPC bridge, whose SCH SMBus lpc_sch exposes
	// as a child platform device. The CPLDs, sensors, fans and cages are
	// all behind it.
	MuxParent string `yaml:"mux_parent"`

	// PSUBus is the PCI function of the iSMT controller carrying the power
	// supplies' EEPROMs and PMBus directly: "0000:00:13.1".
	PSUBus string `yaml:"psu_bus"`

	// GPIOChip is the label prefix of the gpiochip carrying the mux lines:
	// "sch_gpio" on the S1220, whose chip is labelled sch_gpio.<n>.
	GPIOChip string `yaml:"gpio_chip"`

	// FanFloorPercent is the lowest duty SetFanPercent will command. Dell's
	// own fan script idles at 7000 of 19000 RPM, 37%; empty means 40.
	FanFloorPercent int `yaml:"fan_floor_percent"`
}

var pciFunction = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[0-9a-f]{2}\.[0-7]$`)

// Validate checks the block before anything reads a bus through it.
func (d *Data) Validate() error {
	if d == nil {
		return nil
	}
	if !pciFunction.MatchString(d.MuxParent) {
		return fmt.Errorf("mux_parent %q is not a PCI function (0000:00:1f.0)", d.MuxParent)
	}
	if !pciFunction.MatchString(d.PSUBus) {
		return fmt.Errorf("psu_bus %q is not a PCI function (0000:00:13.1)", d.PSUBus)
	}
	if d.MuxParent == d.PSUBus {
		return fmt.Errorf("mux_parent and psu_bus are both %s; they are different controllers", d.MuxParent)
	}
	if d.GPIOChip == "" {
		return fmt.Errorf("gpio_chip is required (sch_gpio on the S1220)")
	}
	if d.FanFloorPercent != 0 && (d.FanFloorPercent < 20 || d.FanFloorPercent > 100) {
		return fmt.Errorf("fan_floor_percent %d is outside 20..100", d.FanFloorPercent)
	}
	return nil
}

func (d *Data) floor() int {
	if d.FanFloorPercent == 0 {
		return 40
	}
	return d.FanFloorPercent
}
