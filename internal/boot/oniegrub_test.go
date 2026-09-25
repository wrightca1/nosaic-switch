package boot

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gptFixture is fixture() with a real GPT disk image, because onie-grub
// installs partitions, not a disk, and has to find them by name.
func gptFixture(t *testing.T, bootMiB int) (Image, string) {
	t.Helper()
	if _, err := exec.LookPath("sfdisk"); err != nil {
		t.Skip("sfdisk not available")
	}
	img, dir := fixture(t)
	if err := os.Truncate(img.Disk, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(img.Disk, int64(bootMiB+2*4+4+2)<<20); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sfdisk", "--quiet", img.Disk)
	cmd.Stdin = strings.NewReader(fmt.Sprintf(`label: gpt
size=%dMiB, type=linux, name="nosaic-boot"
size=4MiB, type=linux, name="nosaic-slot-a"
size=4MiB, type=linux, name="nosaic-slot-b"
type=linux, name="nosaic-data"
`, bootMiB))
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %v\n%s", err, b)
	}
	// Recognisable bytes at the start of boot and data, to check the right
	// partition comes back out of the installer.
	parts, err := namedPartitions(img.Disk)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(img.Disk, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for name, tag := range map[string]string{"nosaic-boot": "BOOT-PART", "nosaic-data": "DATA-PART"} {
		if _, err := f.WriteAt([]byte(tag), parts[name].Start*512); err != nil {
			t.Fatal(err)
		}
	}
	img.KernelParams = "processor.max_cstate=1 iomem=relaxed"
	img.Console = "ttyS0,115200"
	return img, dir
}

func member(t *testing.T, installer, name string) []byte {
	t.Helper()
	script := `set -e
SKIP=$(awk '/^__NOSAIC_PAYLOAD__$/ { print NR + 1; exit 0; }' "$1")
tail -n +$SKIP "$1" | tar -xO "$2"`
	out, err := exec.Command("sh", "-c", script, "sh", installer, name).Output()
	if err != nil {
		t.Fatalf("extracting %s: %v", name, err)
	}
	return out
}

func TestONIEGRUBInstallerIsValidPOSIXShell(t *testing.T) {
	sh, err := exec.LookPath("dash")
	if err != nil {
		if sh, err = exec.LookPath("sh"); err != nil {
			t.Skip("no POSIX shell available to check with")
		}
	}
	script := fmt.Sprintf(installerONIEGRUB, "1.0", "board", "x86_64",
		int64(65536), int64(1048576), int64(1048576), "ttyS0,115200")
	cmd := exec.Command(sh, "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the onie-grub installer is not valid shell: %v\n%s", err, out)
	}
}

func TestONIEGRUBPayloadIsThePartitionsByName(t *testing.T) {
	img, dir := gptFixture(t, 8)
	b, _ := For("onie-grub")
	out, err := b.Wrap(img, dir, io.Discard)
	if err != nil {
		t.Fatalf("wrap: %v", err)
	}
	parts, _ := namedPartitions(img.Disk)
	disk, _ := os.ReadFile(img.Disk)
	for member_, part := range map[string]string{"boot.img.gz": "nosaic-boot", "data.img.gz": "nosaic-data"} {
		gz := member(t, out, member_)
		cmd := exec.Command("gunzip", "-c")
		cmd.Stdin = bytes.NewReader(gz)
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("gunzip %s: %v", member_, err)
		}
		p := parts[part]
		if want := disk[p.Start*512 : (p.Start+p.Size)*512]; !bytes.Equal(raw, want) {
			t.Errorf("%s is not the bytes of %s", member_, part)
		}
	}
	for name, want := range map[string]string{
		"slot-a.sqsh": "SQUASHFS-CONTENT",
		"vmlinuz":     "KERNEL-CONTENT",
		"initrd.img":  "INITRAMFS-CONTENT",
	} {
		if got := string(member(t, out, name)); got != want {
			t.Errorf("%s came back as %q", name, got)
		}
	}
	cfg := string(member(t, out, "grub.cfg"))
	for _, want := range []string{"NOSaic 9.9.9", "--label --set=root nosaic-boot",
		"console=@CONSOLE@ processor.max_cstate=1 iomem=relaxed", "initrd /initrd.img"} {
		if !strings.Contains(cfg, want) {
			t.Errorf("grub.cfg lacks %q:\n%s", want, cfg)
		}
	}
	// Sizes the installer creates must be the sizes the filesystems were
	// built for, in KiB.
	head, _ := os.ReadFile(out)
	if !strings.Contains(string(head), fmt.Sprintf("BOOT_KIB=%d\n", parts["nosaic-boot"].Size/2)) {
		t.Error("BOOT_KIB does not match the image's boot partition")
	}
}

// ⚠ ONIE SHARES THIS DISK. The installer must never write the whole disk and
// must only ever delete partitions it named. Checked as text, because the
// real thing needs a block device and ONIE on it.
func TestONIEGRUBInstallerLeavesONIEAlone(t *testing.T) {
	if strings.Contains(installerONIEGRUB, `of="$DISK"`) {
		t.Error("the installer writes the raw disk; ONIE lives on it")
	}
	if !strings.Contains(installerONIEGRUB, `$NF ~ /^nosaic-/`) {
		t.Error("partition removal is not restricted to nosaic-* names")
	}
	if !strings.Contains(installerONIEGRUB, `LABEL="ONIE-BOOT"`) {
		t.Error("the disk is not found through ONIE's own partition")
	}
}

func TestONIEGRUBRefusesWhatItCannotInstall(t *testing.T) {
	b, _ := For("onie-grub")
	if _, err := b.Wrap(Image{Board: "b", Version: "1"}, t.TempDir(), io.Discard); err == nil {
		t.Error("accepted an image with no artifacts")
	}
	img, dir := fixture(t)
	img.RAMBoot = true
	if _, err := b.Wrap(img, dir, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "RAM-boot") {
		t.Errorf("want a RAM-boot refusal, got %v", err)
	}
}

func TestONIEGRUBRefusesABootPartitionTooSmall(t *testing.T) {
	img, dir := gptFixture(t, 1)
	big := filepath.Join(dir, "big-initrd")
	if err := os.WriteFile(big, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(big, 2<<20); err != nil {
		t.Fatal(err)
	}
	img.Initramfs = big
	b, _ := For("onie-grub")
	if _, err := b.Wrap(img, dir, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "boot_mib") {
		t.Fatalf("want a refusal naming boot_mib, got %v", err)
	}
}

func TestONIEGRUBNetbootIsValidShellAndCarriesTheImages(t *testing.T) {
	img, dir := fixture(t)
	img.RAMBoot = true
	img.Console = "ttyS0,9600"
	img.KernelParams = "processor.max_cstate=1 iomem=relaxed"
	b, _ := For("onie-grub")
	nb, ok := b.(Netbooter)
	if !ok {
		t.Fatal("onie-grub should be able to netboot")
	}
	out, err := nb.Netboot(img, dir, io.Discard)
	if err != nil {
		t.Fatalf("netboot: %v", err)
	}
	bin := filepath.Join(out, "NOSaic-9.9.9-test-board-netboot.bin")
	body, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	script := string(body[:strings.Index(string(body), "\n__NOSAIC_PAYLOAD__\n")])
	if sh, err := exec.LookPath("sh"); err == nil {
		cmd := exec.Command(sh, "-n")
		cmd.Stdin = strings.NewReader(script)
		if o, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("netboot script is not valid shell: %v\n%s", err, o)
		}
	}
	// A netboot must never open the disk.
	for _, bad := range []string{"dd ", "sgdisk", "mkfs", "grub-install", "/dev/sd"} {
		if strings.Contains(script, bad) {
			t.Errorf("the netboot script mentions %q; it must not touch the disk", bad)
		}
	}
	if !strings.Contains(script, "kexec -e") {
		t.Error("the netboot script does not kexec")
	}
	if got := string(member(t, bin, "vmlinuz")); got != "KERNEL-CONTENT" {
		t.Errorf("vmlinuz came back as %q", got)
	}
	if got := string(member(t, bin, "initrd.img")); got != "INITRAMFS-CONTENT" {
		t.Errorf("initrd.img came back as %q", got)
	}
	if c, _ := os.ReadFile(filepath.Join(out, "cmdline")); string(c) != img.KernelParams {
		t.Errorf("cmdline is %q", c)
	}
}

func TestONIEGRUBNetbootRequiresRAMBoot(t *testing.T) {
	img, dir := fixture(t)
	b, _ := For("onie-grub")
	if _, err := b.(Netbooter).Netboot(img, dir, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "--ram-boot") {
		t.Fatalf("want a refusal naming --ram-boot, got %v", err)
	}
}

// memmap=64M$0xb0000000 is the S6000's DMA reservation, and GRUB would eat
// the $ and everything after it.
func TestGRUBEscapesTheDMAReservation(t *testing.T) {
	img, dir := gptFixture(t, 8)
	img.KernelParams = "reboot=p memmap=64M$0xb0000000 iomem=relaxed"
	b, _ := For("onie-grub")
	out, err := b.Wrap(img, dir, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cfg := string(member(t, out, "grub.cfg"))
	if !strings.Contains(cfg, `memmap=64M\$0xb0000000`) {
		t.Errorf("grub.cfg does not escape the $:\n%s", cfg)
	}
	if got := grubEscape(`a\b$c`); got != `a\\b\$c` {
		t.Errorf("grubEscape: %q", got)
	}
}
