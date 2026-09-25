package s6000

import (
	"fmt"
	"os"
	"syscall"
)

// The 74CBTLV3253's channels, as Dell's driver numbers them (SONiC's i2c-10
// through i2c-13).
const (
	chSystem = 0 // CPLDs, CPU and DIMM sensors, the system EEPROM
	chFans   = 1 // fan controllers, board temperature sensors, hot-swap
	chQSFPLo = 2 // cages 1-16, selected by the slave CPLD
	chQSFPHi = 3 // cages 17-32, selected by the master CPLD
)

// The mux lines on the sch_gpio chip: two select bits and a reset.
const (
	gpioSel0  = 1
	gpioSel1  = 2
	gpioReset = 10
)

// lockPath serialises every use of the tree across processes. Replaced in
// tests.
var lockPath = "/run/nosaic-s6000-i2c.lock"

// tree is the mux-parent bus with its GPIO-selected channels.
type tree struct {
	b    bus
	gpio gpio
}

// step is one channel's worth of work in a sequence.
type step struct {
	ch int
	fn func(b bus) error
}

// on runs fn with the mux on channel ch, holding the tree lock throughout.
func (t *tree) on(ch int, fn func(b bus) error) error {
	return t.seq(step{ch, fn})
}

// seq runs steps in order, each on its own channel, under one hold of the
// tree lock -- for work that spans channels, like selecting a cage on the
// CPLD channel and then reading it on the QSFP one.
//
// ⚠ A FAILED SEQUENCE IS RETRIED ONCE, WHOLE, AFTER RESETTING THE MUX. That
// is what Dell's driver does -- it pulses GPIO 10 whenever an SMBus transfer
// fails, then retries -- and its comment says why: this mux wedges. Doing
// less would make the first wedge look like a dead sensor. The whole sequence
// rather than the failed step, because a later step depends on what an
// earlier one selected.
func (t *tree) seq(steps ...step) error {
	unlock, err := lockTree()
	if err != nil {
		return err
	}
	defer unlock()

	run := func() error {
		for _, s := range steps {
			if err := t.selectChannel(s.ch); err != nil {
				return err
			}
			if err := s.fn(t.b); err != nil {
				return err
			}
		}
		return nil
	}
	if err := run(); err == nil {
		return nil
	}
	if err := t.gpio.Set(map[int]int{gpioReset: 1}); err != nil {
		return fmt.Errorf("resetting the i2c mux: %w", err)
	}
	if err := t.gpio.Set(map[int]int{gpioReset: 0}); err != nil {
		return fmt.Errorf("releasing the i2c mux reset: %w", err)
	}
	return run()
}

func (t *tree) selectChannel(ch int) error {
	if ch < 0 || ch > 3 {
		return fmt.Errorf("mux channel %d does not exist", ch)
	}
	// i2c-mux-gpio's convention, which Dell's platform data uses: channel
	// value bit 0 on the first listed GPIO (1), bit 1 on the second (2).
	if err := t.gpio.Set(map[int]int{gpioSel0: ch & 1, gpioSel1: ch >> 1 & 1}); err != nil {
		return fmt.Errorf("selecting i2c mux channel %d: %w", ch, err)
	}
	return nil
}

func lockTree() (func(), error) {
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the i2c tree lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, fmt.Errorf("the i2c tree lock: %w", err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}
