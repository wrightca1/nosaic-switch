package s6000

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"
)

// gpio drives SoC GPIO lines as outputs. Behind an interface for tests.
type gpio interface {
	// Set drives each line (by offset on the chip) to its value.
	Set(lines map[int]int) error
}

var openGPIO = openCdevGPIO

// devDir is where gpiochip character devices appear. Replaced in tests.
var devDir = "/dev"

// The gpio character device, v1 ABI (include/uapi/linux/gpio.h).
//
// v1 rather than v2 because it is two small fixed structs and one ioctl to
// set lines; CONFIG_GPIO_CDEV_V1 is on in NOSaic's kernel. Not the legacy
// /sys/class/gpio: CONFIG_GPIO_SYSFS is off, and its numbering is dynamic on
// 6.x anyway -- Dell's "gpio 10" is line 10 of the sch_gpio chip, which on
// SONiC's 4.19 happened also to be global number 10.
const (
	gpioGetChipInfo   = 0x8044B401 // _IOR(0xB4, 0x01, struct gpiochip_info)
	gpioGetLineHandle = 0xC16CB403 // _IOWR(0xB4, 0x03, struct gpiohandle_request)
	gpioHandleOutput  = 1 << 1
)

type gpiochipInfo struct {
	name  [32]byte
	label [32]byte
	lines uint32
}

type gpiohandleRequest struct {
	lineoffsets   [64]uint32
	flags         uint32
	defaultValues [64]uint8
	consumerLabel [32]byte
	lines         uint32
	fd            int32
}

type cdevGPIO struct{ path string }

// openCdevGPIO finds the chip whose label starts with prefix.
func openCdevGPIO(prefix string) (gpio, error) {
	chips, _ := filepath.Glob(filepath.Join(devDir, "gpiochip*"))
	var seen []string
	for _, c := range chips {
		f, err := os.Open(c)
		if err != nil {
			continue
		}
		var info gpiochipInfo
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), gpioGetChipInfo, uintptr(unsafe.Pointer(&info)))
		f.Close()
		if errno != 0 {
			continue
		}
		label := string(bytes.TrimRight(info.label[:], "\x00"))
		seen = append(seen, label)
		if strings.HasPrefix(label, prefix) {
			return &cdevGPIO{path: c}, nil
		}
	}
	return nil, fmt.Errorf("no gpiochip labelled %s* (found %q); is CONFIG_GPIO_SCH built in?", prefix, seen)
}

// Set requests the lines as outputs at the given values and releases them.
//
// ⚠ REQUESTED AND RELEASED EVERY TIME, NOT HELD. A held line belongs to the
// process holding it, and two things use this tree: the thermal service,
// which runs for as long as the switch does, and every `nosaic platform`
// command. Holding the lines would make the second of those fail with EBUSY.
// Releasing leaves the line where it was driven -- gpio-sch does not reset a
// line on release -- and the tree lock in tree.go keeps the two from
// interleaving.
func (g *cdevGPIO) Set(lines map[int]int) error {
	f, err := os.Open(g.path)
	if err != nil {
		return err
	}
	defer f.Close()
	var req gpiohandleRequest
	i := 0
	for off, v := range lines {
		req.lineoffsets[i] = uint32(off)
		req.defaultValues[i] = uint8(v & 1)
		i++
	}
	req.lines = uint32(i)
	req.flags = gpioHandleOutput
	copy(req.consumerLabel[:], "nosaic-s6000")
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), gpioGetLineHandle, uintptr(unsafe.Pointer(&req)))
	runtime.KeepAlive(&req)
	if errno != 0 {
		return fmt.Errorf("%s: requesting lines %v: %w", g.path, lines, errno)
	}
	return syscall.Close(int(req.fd))
}
