package boot

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Netboot for x86 ONIE is a kexec, not a bootloader fetch.
//
// ONIE's kernel is built with kexec precisely so that an installer can boot a
// kernel of its own (ONIE design spec, "Linux Kernel Configuration"). So the
// netboot artifact is an ONIE installer that installs nothing: ONIE downloads
// it, runs it, and it kexecs into NOSaic with the root filesystem in RAM. The
// disk, ONIE, and whatever NOS is installed are never opened for writing, and
// a power cycle is back at ONIE.
//
// That is the right first contact with a switch whose only recovery path is
// ONIE on the same disk we would otherwise be partitioning.
const netbootONIEGRUB = `#!/bin/sh
# NOSaic netboot for x86 ONIE. Generated -- do not edit.
#
# Run by ONIE (onie-nos-install <url>) or by hand from ONIE's rescue shell.
# Boots NOSaic from RAM with kexec. NOTHING IS WRITTEN TO THE DISK.

set -e
export PATH="/usr/sbin:/sbin:/usr/bin:/bin:$PATH"

echo "NOSaic %s netboot for %s (%s) -- RAM only, the disk is not touched"

BUILD_CONSOLE='%s'
KERNEL_PARAMS='%s'

if ! command -v kexec >/dev/null 2>&1; then
    echo "error: this ONIE has no kexec. Boot the netboot bundle's vmlinuz and" >&2
    echo "       initrd.img from ONIE's GRUB instead; see its README" >&2
    exit 1
fi

SKIP=$(awk '/^__NOSAIC_PAYLOAD__$/ { print NR + 1; exit 0; }' "$0")
DIR=$(mktemp -d)
tail -n +$SKIP "$0" | tar -xf - -C "$DIR"

# The console ONIE booted with is the one this box has.
CONSOLE=$(sed -n 's/.*console=\(ttyS[0-9]*,[0-9]*\).*/\1/p' /proc/cmdline)
[ -n "$CONSOLE" ] || CONSOLE="$BUILD_CONSOLE"
CMDLINE="console=${CONSOLE}n8 $KERNEL_PARAMS"
echo "kernel command line: $CMDLINE"

kexec -l "$DIR/vmlinuz" --initrd="$DIR/initrd.img" --command-line="$CMDLINE"
sync
echo "kexec into NOSaic"
kexec -e

# Only reached if kexec -e returned, which means it did not happen.
echo "error: kexec -e returned; still in ONIE" >&2
exit 1

__NOSAIC_PAYLOAD__
`

const netbootONIEGRUBREADME = `NOSaic %s netboot bundle for %s
================================================================

  %s   ONIE installer that kexecs into NOSaic
  vmlinuz      the kernel
  initrd.img   the initramfs, WITH THE ROOT FILESYSTEM INSIDE IT
  cmdline      the kernel command line, less console=

This is a RAM boot. Nothing is written to the switch's disk: ONIE and any
installed NOS are untouched, and a power cycle returns to ONIE's GRUB.

⚠ NOTHING SURVIVES THE REBOOT. There is no data partition on a RAM boot.

1. From ONIE's install mode (easiest)
-------------------------------------
Serve this directory over HTTP, then on the switch's console, with ONIE in
install or rescue mode:

    onie-nos-install http://<server>/<path>/%s

2. By hand, from ONIE's rescue shell
------------------------------------
    cd /tmp
    wget http://<server>/<path>/vmlinuz http://<server>/<path>/initrd.img
    kexec -l vmlinuz --initrd=initrd.img \
        --command-line="console=ttyS0,<baud>n8 $(cat cmdline)"
    kexec -e

Use the baud rate ONIE itself uses (cat /proc/cmdline).

3. From ONIE's GRUB, if this ONIE has no kexec
----------------------------------------------
Copy vmlinuz and initrd.img to a USB stick (ext2 or FAT), press 'c' at the
GRUB menu, then:

    ls                                  # find the stick, e.g. (hd1,msdos1)
    linux  (hd1,msdos1)/vmlinuz console=ttyS0,<baud>n8 <contents of cmdline>
    initrd (hd1,msdos1)/initrd.img
    boot

Getting back
------------
Power-cycle the switch. On an S6000, prefer a real power cycle over a warm
reboot: its ASIC needs the CPLD power reset to come back cleanly.
`

// Netboot builds the kexec bundle. It refuses an image whose root filesystem
// is on a disk slot, for the same reason uefi does: a netbooted image has no
// slot to find.
func (o onieGRUB) Netboot(img Image, outDir string, log io.Writer) (string, error) {
	if img.Kernel == "" || img.Initramfs == "" {
		return "", fmt.Errorf("a netboot bundle needs a kernel and an initramfs")
	}
	if !img.RAMBoot {
		return "", fmt.Errorf("a netboot image must carry its root filesystem in the " +
			"initramfs, or it will boot to a rescue shell looking for a disk slot " +
			"that does not exist.\n       Rebuild with --ram-boot")
	}
	for _, s := range []string{img.Console, img.KernelParams} {
		if strings.ContainsRune(s, '\'') {
			return "", fmt.Errorf("a single quote in the console or kernel_params would break the netboot script's quoting")
		}
	}

	dir := filepath.Join(outDir, "netboot")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(log, "==> building the ONIE kexec netboot bundle\n")

	for _, f := range [][2]string{{img.Kernel, "vmlinuz"}, {img.Initramfs, "initrd.img"}} {
		if err := copyFileTo(f[0], filepath.Join(dir, f[1])); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"),
		[]byte(strings.TrimSpace(img.KernelParams)), 0o644); err != nil {
		return "", err
	}

	bin := fmt.Sprintf("NOSaic-%s-%s-netboot.bin", img.Version, img.Board)
	readme := fmt.Sprintf(netbootONIEGRUBREADME, img.Version, img.Board, bin, bin)
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte(readme), 0o644); err != nil {
		return "", err
	}

	f, err := os.Create(filepath.Join(dir, bin))
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := fmt.Sprintf(netbootONIEGRUB, img.Version, img.Board, img.Arch,
		img.Console, img.KernelParams)
	if _, err := f.WriteString(head); err != nil {
		return "", err
	}
	cmd := exec.Command("tar", "-cf", "-", "-C", dir, "vmlinuz", "initrd.img")
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("appending the netboot payload: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
