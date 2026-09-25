package s6000

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/salvaged-silicon/nosaic-switch/internal/platformhal"
)

// fakeBoard is the S6000's i2c tree: a GPIO-selected channel on the mux
// parent, and devices that answer only on their own channel -- so a read on
// the wrong channel fails, as it does on the switch.
type fakeBoard struct {
	sel    [3]int                       // GPIO 1, 2 and 10
	regs   map[int]map[int]map[int]byte // channel -> addr -> reg
	writes []string
	resets int
	failN  int // fail this many transfers, to exercise the mux reset
}

func newFakeBoard() *fakeBoard {
	fb := &fakeBoard{regs: map[int]map[int]map[int]byte{0: {}, 1: {}, 2: {}, 3: {}}}
	for _, a := range []int{cpldSystem, cpldMaster, cpldSlave, jc42Addr, emc1403Addr, sysEEPROM} {
		fb.regs[0][a] = map[int]byte{}
	}
	for _, a := range []int{fanCtlA, fanCtlB, 0x4c, 0x4d, 0x4e} {
		fb.regs[1][a] = map[int]byte{}
	}
	fb.regs[2][qsfpEEPROM] = map[int]byte{}
	fb.regs[3][qsfpEEPROM] = map[int]byte{}
	fb.regs[0][cpldSystem][regSystemVersion] = 0x02
	fb.regs[0][cpldMaster][regMasterVersion] = 0x03
	fb.regs[0][cpldSlave][regSlaveVersion] = 0x04
	return fb
}

func (f *fakeBoard) ch() int { return f.sel[0] | f.sel[1]<<1 }

func (f *fakeBoard) Set(lines map[int]int) error {
	for off, v := range lines {
		switch off {
		case gpioSel0:
			f.sel[0] = v
		case gpioSel1:
			f.sel[1] = v
		case gpioReset:
			if v == 1 {
				f.resets++
			}
			f.sel[2] = v
		default:
			return fmt.Errorf("line %d is not one of the mux lines", off)
		}
	}
	return nil
}

func (f *fakeBoard) dev(addr int) (map[int]byte, error) {
	if f.failN > 0 {
		f.failN--
		return nil, errors.New("bus wedged")
	}
	d, ok := f.regs[f.ch()][addr]
	if !ok {
		return nil, fmt.Errorf("nothing at %#02x on channel %d", addr, f.ch())
	}
	return d, nil
}

func (f *fakeBoard) ReadReg(addr, reg int) (byte, error) {
	d, err := f.dev(addr)
	if err != nil {
		return 0, err
	}
	return d[reg], nil
}

func (f *fakeBoard) WriteReg(addr, reg int, v byte) error {
	d, err := f.dev(addr)
	if err != nil {
		return err
	}
	d[reg] = v
	f.writes = append(f.writes, fmt.Sprintf("ch%d %#02x[%#02x]=%#02x", f.ch(), addr, reg, v))
	return nil
}

func (f *fakeBoard) ReadWord(addr, reg int) (uint16, error) {
	d, err := f.dev(addr)
	if err != nil {
		return 0, err
	}
	return uint16(d[reg])<<8 | uint16(d[reg+1]), nil
}

func (f *fakeBoard) ReadAt(addr, off, n int) ([]byte, error) {
	d, err := f.dev(addr)
	if err != nil {
		return nil, err
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = d[off+i]
	}
	return out, nil
}

func (f *fakeBoard) Close() error { return nil }

// psuFake is the other iSMT: nothing behind the mux answers on it.
type psuFake struct{ fakeBoard }

func open(t *testing.T, fb *fakeBoard) *HAL {
	t.Helper()
	lockPath = filepath.Join(t.TempDir(), "lock")
	openBus = func(pci string) (bus, error) {
		if pci == "0000:00:13.1" {
			return &psuFake{}, nil
		}
		return fb, nil
	}
	openGPIO = func(string) (gpio, error) { return fb, nil }
	h, err := Open(platformhal.Config{BoardData: &Data{
		MuxParent: "0000:00:13.0", PSUBus: "0000:00:13.1", GPIOChip: "sch_gpio"}})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return h
}

func TestDecoders(t *testing.T) {
	for _, c := range []struct {
		got, want int
		what      string
	}{
		{tmp75MilliC(0x1900), 25000, "tmp75 25 C"},
		{tmp75MilliC(0x1980), 25500, "tmp75 25.5 C"},
		{tmp75MilliC(0xe700), -25000, "tmp75 -25 C"},
		{jc42MilliC(0x0190), 25000, "jc42 25 C"},
		{jc42MilliC(0xe190), 25000, "jc42 alarm bits ignored"},
		{jc42MilliC(0x1ff0), -1000, "jc42 -1 C"},
		{emc1403MilliC(45, 0xa0), 45625, "emc1403 45.625 C"},
		{max6620Div(0x30), 2, "max6620 divider"},
	} {
		if c.got != c.want {
			t.Errorf("%s: got %d want %d", c.what, c.got, c.want)
		}
	}
	hi, lo := max6620Target(2, 15000)
	if n := max6620Count(hi, lo); n != 32 {
		t.Errorf("15000 RPM at div 2: count %d, want 32", n)
	}
	if r := max6620RPM(2, 0x7ff); r != 0 {
		t.Errorf("a saturated count is a stopped fan, got %d RPM", r)
	}
}

func TestPSUAndTrayBits(t *testing.T) {
	for _, c := range []struct {
		reg      byte
		psu      int
		pres, ok bool
	}{
		{0x00, 1, true, true}, {0x00, 2, true, true},
		{0x80, 1, false, false}, {0x40, 1, true, false},
		{0x08, 2, false, false}, {0x04, 2, true, false}, {0x08, 1, true, true},
	} {
		p, ok := psuNibble(c.reg, c.psu)
		if p != c.pres || ok != c.ok {
			t.Errorf("reg %#02x PSU%d: present=%v ok=%v, want %v %v", c.reg, c.psu, p, ok, c.pres, c.ok)
		}
	}
	if got := fanTrays(0x40, 0x00); got != [3]bool{true, false, false} {
		t.Errorf("0x08=0x40: %v, want tray 1 only", got)
	}
	if got := fanTrays(0xc0, 0x01); got != [3]bool{true, true, true} {
		t.Errorf("all trays: %v", got)
	}
}

func dellEEPROM() []byte {
	e := make([]byte, 128)
	hdr := func(off, size int, code byte) {
		copy(e[off:], []byte{0x3a, 0x29, byte(size), byte(size >> 8), code, 1})
	}
	hdr(0x00, 64, 0x20)
	copy(e[6:], "CN0ABCDE1234567890AB") // PPID
	copy(e[26:], "A01")                 // DPN rev
	copy(e[29:], "SVCTAG1")             // service tag
	copy(e[36:], "0W1YV0    ")          // part number
	hdr(0x40, 48, 0x1f)
	hdr(0x70, 16, 0x21)
	copy(e[0x76:], []byte{0x00, 0x1e, 0x4f, 0x12, 0x34, 0x56})
	return e
}

func TestIdentity(t *testing.T) {
	fb := newFakeBoard()
	for i, b := range dellEEPROM() {
		fb.regs[0][sysEEPROM][i] = b
	}
	h := open(t, fb)
	id, err := h.Board()
	if err != nil {
		t.Fatal(err)
	}
	if id.Serial != "SVCTAG1" || id.Revision != "A01" || id.Model != "Dell S6000-ON 0W1YV0" ||
		id.SID != "CN0ABCDE1234567890AB" || id.MAC.String() != "00:1e:4f:12:34:56" {
		t.Errorf("identity %+v", id)
	}
	e := dellEEPROM()
	e[0] = 0xff
	if _, _, _, _, _, err := dellIdentity(e); err == nil {
		t.Error("a block with a bad magic was accepted")
	}
}

func TestOpenRefusesAMissingCPLD(t *testing.T) {
	fb := newFakeBoard()
	delete(fb.regs[0], cpldSystem)
	lockPath = filepath.Join(t.TempDir(), "lock")
	openBus = func(string) (bus, error) { return fb, nil }
	openGPIO = func(string) (gpio, error) { return fb, nil }
	_, err := Open(platformhal.Config{BoardData: &Data{
		MuxParent: "0000:00:13.0", PSUBus: "0000:00:13.1", GPIOChip: "sch_gpio"}})
	if err == nil || !strings.Contains(err.Error(), "the other way round") {
		t.Fatalf("want a refusal suggesting the buses are swapped, got %v", err)
	}
}

func TestTemperaturesReadEachSensorOnItsChannel(t *testing.T) {
	fb := newFakeBoard()
	fb.regs[1][0x4c][0], fb.regs[1][0x4c][1] = 0x2d, 0x00 // 45 C
	fb.regs[1][0x4d][0] = 0x1e                            // 30 C
	fb.regs[1][0x4e][0] = 0x19                            // 25 C
	fb.regs[0][emc1403Addr][0x00] = 50
	fb.regs[0][emc1403Addr][0x01] = 48
	fb.regs[0][emc1403Addr][0x23] = 47
	fb.regs[0][jc42Addr][5], fb.regs[0][jc42Addr][6] = 0x01, 0x90 // 25 C
	h := open(t, fb)
	got, err := h.Temperatures()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"ASIC": 45000, "NIC": 30000, "Ambient": 25000,
		"CPU": 50000, "CPU0": 48000, "CPU1": 47000, "DIMM": 25000}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d (all: %v)", k, got[k], v, got)
		}
	}
}

func TestPSUPresent(t *testing.T) {
	fb := newFakeBoard()
	fb.regs[0][cpldMaster][regPSU] = 0x0c // PSU2 absent
	h := open(t, fb)
	got, err := h.PSUPresent()
	if err != nil {
		t.Fatal(err)
	}
	if !got["PSU1"] || got["PSU2"] {
		t.Errorf("%v, want PSU1 only", got)
	}
}

func TestReadModuleSelectsTheCageThenReadsItsChannel(t *testing.T) {
	fb := newFakeBoard()
	fb.regs[3][qsfpEEPROM][0] = 0x0d // QSFP+ identifier
	h := open(t, fb)
	var slept time.Duration
	sleep = func(d time.Duration) { slept += d }
	defer func() { sleep = time.Sleep }()
	fb.writes = nil
	got, err := h.ReadModuleBytes(20, 0x50, -1, 0, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got[0] != 0x0d {
		t.Errorf("read %#02x, want the cage's 0x0d", got[0])
	}
	// Cage 20 is bit 3 of the master CPLD's group: ~(1<<3) = 0xfff7.
	want := []string{"ch0 0x32[0x0a]=0xf7", "ch0 0x32[0x0b]=0xff"}
	if strings.Join(fb.writes, ",") != strings.Join(want, ",") {
		t.Errorf("writes %v, want %v", fb.writes, want)
	}
	if slept < 2*time.Millisecond {
		t.Errorf("no settle after the cage select (slept %v); Dell waits 2 ms", slept)
	}
	if _, err := h.ReadModuleBytes(1, 0x51, -1, 0, 1); !errors.Is(err, platformhal.ErrUnsupported) {
		t.Errorf("0x51 on a QSFP board: %v", err)
	}
	if _, err := h.ReadModuleBytes(33, 0x50, -1, 0, 1); err == nil {
		t.Error("cage 33 was accepted")
	}
}

func TestReadModuleWritesThePageSelect(t *testing.T) {
	fb := newFakeBoard()
	h := open(t, fb)
	fb.writes = nil
	if _, err := h.ReadModuleBytes(2, 0x50, 3, 128, 4); err != nil {
		t.Fatal(err)
	}
	if last := fb.writes[len(fb.writes)-1]; last != "ch2 0x50[0x7f]=0x03" {
		t.Errorf("last write %q, want page 3 selected on channel 2", last)
	}
}

func TestPowerCycleWritesTheSystemCPLD(t *testing.T) {
	fb := newFakeBoard()
	h := open(t, fb)
	fb.writes = nil
	if err := h.PowerCycle(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fb.writes, ",") != "ch0 0x31[0x01]=0xfd" {
		t.Errorf("writes %v", fb.writes)
	}
}

func TestSetFanPercentClampsToTheFloorAndSetsRPMMode(t *testing.T) {
	fb := newFakeBoard()
	h := open(t, fb)
	refused, err := h.SetFanPercent(0)
	if err != nil || refused != 0 {
		t.Fatalf("refused %d: %v", refused, err)
	}
	if c := fb.regs[1][fanCtlA][m6620Conf0]; c&0xa8 != 0xa8 {
		t.Errorf("fan 1 config %#02x is not in RPM mode", c)
	}
	fs, err := h.Fans()
	if err != nil {
		t.Fatal(err)
	}
	if len(fs) != 6 {
		t.Fatalf("%d fans", len(fs))
	}
	for _, f := range fs {
		if f.Percent < h.FanFloorPercent()-2 || f.Percent > h.FanFloorPercent()+2 {
			t.Errorf("fan %d commanded %d%%, want the %d%% floor", f.Index, f.Percent, h.FanFloorPercent())
		}
	}
}

func TestAWedgedMuxIsResetAndRetried(t *testing.T) {
	fb := newFakeBoard()
	h := open(t, fb)
	fb.failN = 1
	if _, err := h.PSUPresent(); err != nil {
		t.Fatalf("one failed transfer should be retried after a mux reset: %v", err)
	}
	if fb.resets != 1 {
		t.Errorf("mux reset %d times, want 1", fb.resets)
	}
}

func TestReleaseSwitchChipPulsesCageReset(t *testing.T) {
	fb := newFakeBoard()
	h := open(t, fb)
	fb.writes = nil
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // do not wait the second
	_ = h.ReleaseSwitchChip(ctx)
	s := strings.Join(fb.writes, ",")
	for _, w := range []string{"ch0 0x33[0x02]=0x00", "ch0 0x33[0x06]=0x00", "ch0 0x32[0x11]=0xff"} {
		if !strings.Contains(s, w) {
			t.Errorf("missing %s in %s", w, s)
		}
	}
	if !strings.HasSuffix(s, "ch0 0x32[0x11]=0xff") {
		t.Errorf("reset must be released last: %s", s)
	}
}

func TestDataValidate(t *testing.T) {
	for _, d := range []Data{
		{MuxParent: "00:13.0", PSUBus: "0000:00:13.1", GPIOChip: "sch_gpio"},
		{MuxParent: "0000:00:13.0", PSUBus: "0000:00:13.0", GPIOChip: "sch_gpio"},
		{MuxParent: "0000:00:13.0", PSUBus: "0000:00:13.1"},
		{MuxParent: "0000:00:13.0", PSUBus: "0000:00:13.1", GPIOChip: "sch_gpio", FanFloorPercent: 5},
	} {
		if err := d.Validate(); err == nil {
			t.Errorf("%+v was accepted", d)
		}
	}
}
