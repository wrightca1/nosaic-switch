package s6000

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// bus is the i2c this HAL needs, behind an interface so everything above it
// can be tested without a switch. The same shape as the AS4610's, and
// duplicated rather than shared for the same reason that package gives: it is
// a handful of ioctls, and a shared i2c layer would be a package with two
// callers and no design of its own.
type bus interface {
	// ReadReg reads one byte from a register.
	ReadReg(addr, reg int) (byte, error)
	// WriteReg writes one byte to a register.
	WriteReg(addr, reg int, v byte) error
	// ReadWord reads two bytes, big-endian on the wire: every part here
	// sends its temperature most-significant byte first, and SMBus's own
	// word transaction would hand them back swapped.
	ReadWord(addr, reg int) (uint16, error)
	// ReadAt reads n bytes from an 8-bit offset with a repeated start.
	ReadAt(addr, offset, n int) ([]byte, error)
	Close() error
}

// openBus opens the i2c adapter under a PCI function. Replaced in tests.
var openBus = openPCIBus

// sysfs is where adapters are found. Replaced in tests.
var sysfs = "/sys"

// adapterFor finds the i2c-N a PCI function provides.
//
// By PCI function rather than by number, because the number is probe order:
// ONIE reaches the system CPLD on i2c-0 and SONiC on i2c-10, on the same box.
// And not by name, because both iSMT functions call themselves "SMBus iSMT
// adapter at <address>", which differs only in an I/O address nobody states.
func adapterFor(pci string) (int, error) {
	dir := filepath.Join(sysfs, "bus", "pci", "devices", pci)
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("PCI function %s: %w", pci, err)
	}
	for _, e := range ents {
		if n, ok := strings.CutPrefix(e.Name(), "i2c-"); ok {
			if v, err := strconv.Atoi(n); err == nil {
				return v, nil
			}
		}
	}
	return 0, fmt.Errorf("PCI function %s has no i2c adapter; is CONFIG_I2C_ISMT built in?", pci)
}

func openPCIBus(pci string) (bus, error) {
	n, err := adapterFor(pci)
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(fmt.Sprintf("/dev/i2c-%d", n), os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("i2c-%d (%s): %w (is CONFIG_I2C_CHARDEV set?)", n, pci, err)
	}
	return &devBus{f: f, n: n, cur: -1}, nil
}

// The ioctls, from Linux's include/uapi/linux/i2c-dev.h. I2C_SLAVE_FORCE
// because nothing in the kernel should be bound behind this mux, and if
// something is, refusing to talk would hide that rather than fix it.
const (
	i2cSlaveForce = 0x0706
	i2cRdwr       = 0x0707
	i2cMRd        = 0x0001
)

type devBus struct {
	f   *os.File
	n   int
	mu  sync.Mutex
	cur int
}

func (b *devBus) Close() error { return b.f.Close() }

func (b *devBus) selectAddr(addr int) error {
	if b.cur == addr {
		return nil
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, b.f.Fd(), i2cSlaveForce, uintptr(addr))
	if errno != 0 {
		return fmt.Errorf("i2c-%d: selecting %#02x: %w", b.n, addr, errno)
	}
	b.cur = addr
	return nil
}

// i2c_msg and i2c_rdwr_ioctl_data. The padding is for the pointer's
// alignment on 64-bit; this board is x86-64 only.
type i2cMsg struct {
	addr  uint16
	flags uint16
	len   uint16
	_     uint16
	buf   uintptr
}

type i2cRdwrData struct {
	msgs  uintptr
	nmsgs uint32
	_     uint32
}

func (b *devBus) ReadAt(addr, offset, n int) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	off := []byte{byte(offset)}
	out := make([]byte, n)
	msgs := [2]i2cMsg{
		{addr: uint16(addr), len: 1, buf: uintptr(unsafe.Pointer(&off[0]))},
		{addr: uint16(addr), flags: i2cMRd, len: uint16(n), buf: uintptr(unsafe.Pointer(&out[0]))},
	}
	data := i2cRdwrData{msgs: uintptr(unsafe.Pointer(&msgs[0])), nmsgs: 2}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, b.f.Fd(), i2cRdwr, uintptr(unsafe.Pointer(&data)))
	runtime.KeepAlive(&off)
	runtime.KeepAlive(&out)
	runtime.KeepAlive(&msgs)
	if errno != 0 {
		return nil, fmt.Errorf("i2c-%d: reading %d byte(s) at %#02x offset %#02x: %w", b.n, n, addr, offset, errno)
	}
	return out, nil
}

func (b *devBus) ReadReg(addr, reg int) (byte, error) {
	v, err := b.ReadAt(addr, reg, 1)
	if err != nil {
		return 0, err
	}
	return v[0], nil
}

func (b *devBus) ReadWord(addr, reg int) (uint16, error) {
	v, err := b.ReadAt(addr, reg, 2)
	if err != nil {
		return 0, err
	}
	return uint16(v[0])<<8 | uint16(v[1]), nil
}

func (b *devBus) WriteReg(addr, reg int, v byte) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.selectAddr(addr); err != nil {
		return err
	}
	if _, err := b.f.Write([]byte{byte(reg), v}); err != nil {
		return fmt.Errorf("i2c-%d: writing %#02x to %#02x register %#02x: %w", b.n, v, addr, reg, err)
	}
	return nil
}
