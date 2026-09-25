package s6000

import (
	"fmt"
	"net"
	"strings"
)

// Addresses, from Dell's s6000_platform.sh and dell_s6000_platform.c.
const (
	cpldSystem = 0x31 // channel 0
	cpldMaster = 0x32 // channel 0; cages 17-32, PSU and fan status, LEDs
	cpldSlave  = 0x33 // channel 0; cages 1-16

	jc42Addr    = 0x18 // channel 0, DIMM
	emc1403Addr = 0x4d // channel 0, CPU
	sysEEPROM   = 0x53 // channel 0, Dell-format board EEPROM

	fanCtlA = 0x29 // channel 1, max6620 fans 1-4
	fanCtlB = 0x2a // channel 1, max6620 fans 5-6 (its channels 3-4 unused)

	qsfpEEPROM = 0x50 // behind each cage's CPLD select
)

// The board temperature sensors on channel 1, as Dell's sensors.conf names
// them, and the tmp75's own address.
var tmp75s = []struct {
	name string
	addr int
}{
	{"ASIC", 0x4c},    // "close to Networking ASIC"
	{"NIC", 0x4d},     // "close to NIC"
	{"Ambient", 0x4e}, // "an ambient temperature sensor"
}

// tmp75MilliC decodes an LM75-family temperature register: a 16-bit
// two's-complement value in 1/256 °C, of which a tmp75 fills 9 to 12 bits.
// lm75.c's lm75_reg_to_mc with 16 bits of resolution is the same thing.
func tmp75MilliC(reg uint16) int {
	return int(int16(reg)) * 1000 / 256
}

// jc42MilliC decodes a JEDEC JC-42.4 temperature register: 13 bits of
// two's complement in 1/16 °C, the top three bits being alarm flags. jc42.c
// sign-extends from bit 12 and scales by 125/2.
func jc42MilliC(reg uint16) int {
	v := int(reg & 0x1fff)
	if v&0x1000 != 0 {
		v -= 0x2000
	}
	return v * 125 / 2
}

// emc1403 channels: high byte (whole °C), low byte (top 3 bits, 1/8 °C).
// The register pairs are emc1403.c's temp_input and its low-byte table.
var emc1403Channels = []struct {
	name     string
	hi, lo   int
	describe string
}{
	{"CPU", 0x00, 0x29, "internal: the hottest of the S1220's dies"},
	{"CPU0", 0x01, 0x10, "remote diode 1"},
	{"CPU1", 0x23, 0x24, "remote diode 2"},
}

func emc1403MilliC(hi, lo byte) int {
	return int(int8(hi))*1000 + int(lo>>5)*125
}

// psuNibble is one supply's half of master CPLD register 0x03: PSU1 the
// high nibble, PSU2 the low. Bit 3 low means present, bit 2 low means not
// failed -- dell_s6000_platform.c's psu0_prs/psu0_status and SONiC's psu.py,
// which calls the supply healthy only with both bits clear.
func psuNibble(reg byte, psu int) (present, ok bool) {
	n := reg
	if psu == 1 {
		n = reg >> 4
	}
	present = n&0x8 == 0
	ok = n&0xc == 0
	return
}

// fanTrays decodes the three trays' presence: master CPLD 0x08 bits 7:6 are
// trays 1 and 2, 0x09 bit 0 is tray 3, set meaning present.
func fanTrays(r8, r9 byte) [3]bool {
	return [3]bool{r8&0x40 != 0, r8&0x80 != 0, r9&0x01 != 0}
}

// max6620 tachometer arithmetic, from max6620.c: an 11-bit count of an
// 8192 Hz clock over `div` fan revolutions at 2 pulses per revolution.
const (
	max6620Clock = 8192
	max6620Pulse = 2
	max6620Min   = 240
	max6620Max   = 30000
)

func max6620Div(dyn byte) int { return 1 << ((dyn & 0xe0) >> 5) }

func max6620Count(hi, lo byte) int { return int(hi)<<3&0x7f8 | int(lo)>>5&0x7 }

func max6620RPM(div, count int) int {
	if count == 0 || count == 0x7ff {
		return 0 // stopped or no pulses: the count saturates
	}
	return 60 * div * max6620Clock / (count * max6620Pulse)
}

func max6620Target(div, rpm int) (hi, lo byte) {
	if rpm < max6620Min {
		rpm = max6620Min
	}
	if rpm > max6620Max {
		rpm = max6620Max
	}
	c := 60 * div * max6620Clock / (rpm * max6620Pulse)
	return byte(c >> 3 & 0xff), byte(c << 5 & 0xe0)
}

// dellIdentity decodes the S6000's 128-byte board EEPROM: three blocks, each
// a 6-byte header (magic 3a 29, little-endian size, block code, revision 1)
// then fixed ASCII fields -- SONiC's EepromS6000. The per-block CRC is not
// checked; a block with a bad header is refused.
func dellIdentity(e []byte) (model, serial, rev, ppid string, mac net.HardwareAddr, err error) {
	if len(e) < 128 {
		return "", "", "", "", nil, fmt.Errorf("board EEPROM: %d bytes, want 128", len(e))
	}
	block := func(off, size int, code byte) ([]byte, error) {
		b := e[off : off+size]
		if b[0] != 0x3a || b[1] != 0x29 || int(b[2])|int(b[3])<<8 != size || b[4] != code || b[5] != 1 {
			return nil, fmt.Errorf("board EEPROM block %#02x at %#02x has no valid header", code, off)
		}
		return b[6:], nil
	}
	field := func(b []byte, from, n int) string {
		return strings.TrimRight(strings.TrimSpace(string(b[from:from+n])), "\x00\xff")
	}
	mfg, err := block(0x00, 64, 0x20)
	if err != nil {
		return "", "", "", "", nil, err
	}
	// PPID 20, DPN Rev 3, Service Tag 7, Part Number 10, Part Rev 3.
	ppid = field(mfg, 0, 20)
	rev = field(mfg, 20, 3)
	serial = field(mfg, 23, 7)
	model = field(mfg, 30, 10)
	if m, err := block(0x70, 16, 0x21); err == nil {
		mac = net.HardwareAddr(append([]byte(nil), m[0:6]...))
	}
	return model, serial, rev, ppid, mac, nil
}
