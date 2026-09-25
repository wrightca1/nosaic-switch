package boot

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func init() { register(onieGRUB{}) }

// onieGRUB is ONIE on x86: a BIOS, GRUB, and ONIE sharing one disk with us.
//
// # Why this is not onie-sfx
//
// onie-sfx was written for the AS5610, where ONIE lives in U-Boot's flash and
// the disk is the NOS's to replace, so it dd's a whole disk image. On x86 ONIE
// lives ON the disk -- GRUB-BOOT and ONIE-BOOT are its first partitions -- and
// dd'ing over them removes the only way back to an installer. So this backend
// does what every x86 ONIE NOS does, modelled directly on SONiC's
// installer/install.sh: it adds partitions of its own beside ONIE's,
// installs GRUB, and writes a grub.cfg that boots NOSaic and still offers
// ONIE's menu entries.
//
// # What makes that safe for A/B
//
// Four partitions, found by their GPT names and never by position. The
// initramfs reads PARTNAME from sysfs and the upgrade code refuses a disk
// whose NOSaic partitions are incomplete, because on this disk "partition 2"
// is ONIE-BOOT.
//
// # What it does not do yet
//
// The kernel and initramfs are copied onto nosaic-boot at install time and
// are shared by both slots, as on the uefi backend. An A/B upgrade replaces
// the root filesystem and not the kernel.
//
// Legacy BIOS only. ONIE on UEFI hardware needs grub-install for x86_64-efi
// and an efibootmgr entry; SONiC does both and it is the obvious next step,
// but it has not been run here, so the installer refuses rather than guesses.
type onieGRUB struct{}

func (onieGRUB) ID() string { return "onie-grub" }

func (onieGRUB) Tools() []string { return []string{"tar", "sfdisk"} }

func (onieGRUB) Describe() string {
	return "an x86 ONIE installer: adds NOSaic's partitions beside ONIE's and installs GRUB, as SONiC does: onie-nos-install <file>"
}

// installerONIEGRUB runs under ONIE on the switch. ONIE x86 carries sgdisk,
// partprobe, blkid, mkfs and grub-install because its own installers need
// them; nothing else is assumed.
const installerONIEGRUB = `#!/bin/sh
# NOSaic installer for x86 ONIE. Generated -- do not edit.
#
# ONIE executes this file directly. Everything below the marker is a tar
# archive appended to this script.

set -e
export PATH="/usr/sbin:/sbin:/usr/bin:/bin:$PATH"

echo "NOSaic %s for %s (%s)"

BOOT_KIB=%d
SLOT_KIB=%d
DATA_KIB=%d
BUILD_CONSOLE='%s'

SKIP=$(awk '/^__NOSAIC_PAYLOAD__$/ { print NR + 1; exit 0; }' "$0")
payload() { tail -n +$SKIP "$0" | tar -xO "$1"; }

if [ -d /sys/firmware/efi ]; then
    echo "error: this switch booted ONIE through UEFI; only legacy BIOS GRUB is" >&2
    echo "       supported by this installer so far" >&2
    exit 1
fi

# ── The disk ONIE lives on ───────────────────────────────────────────────────
#
# Found the way SONiC finds it: by ONIE's own filesystem label. That is the
# disk ONIE boots from, which is the disk its GRUB -- and so ours -- is on.
ONIE_PART=$(blkid | grep 'LABEL="ONIE-BOOT"' | head -n 1 | cut -d: -f1)
if [ -z "$ONIE_PART" ]; then
    echo "error: no ONIE-BOOT partition found; is this an x86 ONIE switch?" >&2
    exit 1
fi
DISK=$(echo "$ONIE_PART" | sed -e 's/[0-9]*$//' -e 's/\([0-9]\)p$/\1/')
case "$DISK" in
    *[0-9]) PSEP=p ;;
    *)      PSEP= ;;
esac
partdev() { echo "$DISK$PSEP$1"; }
echo "ONIE is on $ONIE_PART; installing beside it on $DISK"

if ! sgdisk -p "$DISK" >/dev/null 2>&1 || sgdisk -p "$DISK" 2>&1 | grep -q 'MBR: MBR only'; then
    echo "error: $DISK is not GPT. ONIE x86 normally is; this layout is not supported" >&2
    exit 1
fi

# ── Remove a previous NOSaic ─────────────────────────────────────────────────
#
# Only partitions WE named. Anything else on this disk -- ONIE, a diag
# partition, another NOS -- belongs to somebody else.
for n in $(sgdisk -p "$DISK" | awk '$1 ~ /^[0-9]+$/ && $NF ~ /^nosaic-/ { print $1 }'); do
    d=$(partdev $n)
    umount "$d" 2>/dev/null || true
    echo "removing old NOSaic partition $n"
    sgdisk -d $n "$DISK" >/dev/null
done
partprobe "$DISK" 2>/dev/null || true

# ── Room ─────────────────────────────────────────────────────────────────────
NEED=$(( (BOOT_KIB + 2 * SLOT_KIB + DATA_KIB) * 2 ))
FIRST=$(sgdisk -F "$DISK")
LAST=$(sgdisk -E "$DISK")
FREE=$(( LAST - FIRST + 1 ))
if [ "$FREE" -lt "$NEED" ]; then
    echo "error: NOSaic needs $((NEED / 2048)) MiB free after ONIE and the largest" >&2
    echo "       free block on $DISK is $((FREE / 2048)) MiB." >&2
    echo "       Another NOS is probably using the disk. ONIE's 'Uninstall OS'" >&2
    echo "       menu entry removes it and keeps ONIE." >&2
    exit 1
fi

# ── Partitions ───────────────────────────────────────────────────────────────
nextfree() {
    used=$(sgdisk -p "$DISK" | awk '$1 ~ /^[0-9]+$/ { print $1 }')
    n=1
    while echo "$used" | grep -qx "$n"; do n=$((n + 1)); done
    echo $n
}
mkpart() {
    n=$(nextfree)
    sgdisk --new=$n:0:+$2K --change-name=$n:$1 "$DISK" >/dev/null
    echo "$1 is partition $n" >&2
    echo $n
}
P_BOOT=$(mkpart nosaic-boot   $BOOT_KIB)
P_A=$(mkpart    nosaic-slot-a $SLOT_KIB)
P_B=$(mkpart    nosaic-slot-b $SLOT_KIB)
P_DATA=$(mkpart nosaic-data   $DATA_KIB)

partprobe "$DISK" 2>/dev/null || true
sleep 2
for n in $P_BOOT $P_A $P_B $P_DATA; do
    if [ ! -b "$(partdev $n)" ]; then
        echo "error: $(partdev $n) did not appear after partitioning; not writing" >&2
        exit 1
    fi
done

echo "writing nosaic-boot";   payload boot.img.gz | gunzip -c | dd of="$(partdev $P_BOOT)" bs=1M conv=fsync 2>/dev/null
echo "writing nosaic-slot-a"; payload slot-a.sqsh | dd of="$(partdev $P_A)" bs=1M conv=fsync 2>/dev/null
echo "clearing nosaic-slot-b"; dd if=/dev/zero of="$(partdev $P_B)" bs=1M count=1 conv=fsync 2>/dev/null
echo "writing nosaic-data";   payload data.img.gz | gunzip -c | dd of="$(partdev $P_DATA)" bs=1M conv=fsync 2>/dev/null
sync

# ── Kernel and GRUB ──────────────────────────────────────────────────────────
MNT=$(mktemp -d)
mount -t ext2 "$(partdev $P_BOOT)" "$MNT"
payload vmlinuz    > "$MNT/vmlinuz"
payload initrd.img > "$MNT/initrd.img"

# GRUB goes into the MBR and the BIOS boot partition, with its modules and
# grub.cfg on nosaic-boot -- exactly as SONiC points it at its own partition.
grub-install --boot-directory="$MNT" --recheck "$DISK"

# The console ONIE itself is using is the one this box really has, whatever
# the board file guessed.
CONSOLE=$(sed -n 's/.*console=\(ttyS[0-9]*,[0-9]*\).*/\1/p' /proc/cmdline)
[ -n "$CONSOLE" ] || CONSOLE="$BUILD_CONSOLE"
UNIT=$(echo "$CONSOLE" | sed 's/^ttyS\([0-9]*\),.*/\1/')
SPEED=$(echo "$CONSOLE" | sed 's/^.*,//')
echo "console $CONSOLE"

export GRUB_SERIAL_COMMAND="serial --unit=$UNIT --speed=$SPEED --word=8 --parity=no --stop=1"
export GRUB_CMDLINE_LINUX="console=ttyS$UNIT,${SPEED}n8"

{
    echo "$GRUB_SERIAL_COMMAND"
    echo "terminal_input serial console"
    echo "terminal_output serial console"
    payload grub.cfg | sed -e "s/@CONSOLE@/ttyS$UNIT,${SPEED}n8/"
    # ONIE's own entries, from ONIE's own fragment, so "ONIE: Install OS"
    # and "ONIE: Rescue" stay one menu choice away.
    if [ -x /mnt/onie-boot/onie/grub.d/50_onie_grub ]; then
        /mnt/onie-boot/onie/grub.d/50_onie_grub
    else
        echo "warning: ONIE's grub fragment is missing; the menu will not offer ONIE" >&2
    fi
} > "$MNT/grub/grub.cfg"

umount "$MNT"
rmdir "$MNT"
sync

echo "NOSaic installed."
exit 0

__NOSAIC_PAYLOAD__
`

// grubEntry is the NOSaic half of grub.cfg; the installer adds the console
// and ONIE's entries around it. @CONSOLE@ is filled in on the switch.
const grubEntry = `set timeout=5
set default=0

# grub-reboot and "boot ONIE once", as ONIE's tools expect.
if [ -s $prefix/grubenv ]; then
    load_env
fi
if [ "${next_entry}" ]; then
    set default="${next_entry}"
    unset next_entry
    save_env next_entry
fi
if [ "${onie_entry}" ]; then
    set next_entry="${default}"
    set default="${onie_entry}"
    unset onie_entry
    save_env onie_entry next_entry
fi

menuentry 'NOSaic %s' {
    insmod part_gpt
    insmod ext2
    insmod gzio
    search --no-floppy --label --set=root nosaic-boot
    linux  /vmlinuz console=@CONSOLE@ %s
    initrd /initrd.img
}
`

func (o onieGRUB) Wrap(img Image, outDir string, log io.Writer) (string, error) {
	if img.RAMBoot {
		return "", fmt.Errorf("a RAM-boot image is not installable; build without --ram-boot")
	}
	if img.Disk == "" || img.Squashfs == "" || img.Kernel == "" || img.Initramfs == "" {
		return "", fmt.Errorf("onie-grub needs a disk image, a squashfs, a kernel and an initramfs")
	}
	parts, err := namedPartitions(img.Disk)
	if err != nil {
		return "", err
	}
	for _, n := range []string{"nosaic-boot", "nosaic-slot-a", "nosaic-slot-b", "nosaic-data"} {
		if _, ok := parts[n]; !ok {
			return "", fmt.Errorf("%s has no %q partition; onie-grub needs a GPT image", img.Disk, n)
		}
	}
	boot, slot, data := parts["nosaic-boot"], parts["nosaic-slot-a"], parts["nosaic-data"]

	// The boot partition carries the kernel, the initramfs and GRUB's own
	// modules on top of the slot pointer. Checked here, because on the switch
	// it fails as "No space left on device" halfway through an install.
	var need int64 = 4 << 20 // GRUB's i386-pc modules and slack
	for _, f := range []string{img.Kernel, img.Initramfs} {
		fi, err := os.Stat(f)
		if err != nil {
			return "", err
		}
		need += fi.Size()
	}
	if need > boot.Size*512 {
		return "", fmt.Errorf("the kernel, initramfs and GRUB need %d MiB and nosaic-boot is %d MiB; "+
			"raise boot_mib in platform/%s/board.yml", (need>>20)+1, boot.Size*512>>20, img.Board)
	}

	if strings.ContainsRune(img.Console, '\'') {
		return "", fmt.Errorf("a single quote in the console would break the installer's quoting")
	}

	work := filepath.Join(outDir, "onie-grub")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return "", err
	}
	fmt.Fprintf(log, "==> building the x86 ONIE installer\n")

	if err := extractGz(img.Disk, boot, filepath.Join(work, "boot.img.gz")); err != nil {
		return "", err
	}
	if err := extractGz(img.Disk, data, filepath.Join(work, "data.img.gz")); err != nil {
		return "", err
	}
	for _, f := range [][2]string{
		{img.Squashfs, "slot-a.sqsh"},
		{img.Kernel, "vmlinuz"},
		{img.Initramfs, "initrd.img"},
	} {
		if err := copyFileTo(f[0], filepath.Join(work, f[1])); err != nil {
			return "", err
		}
	}
	cfg := fmt.Sprintf(grubEntry, img.Version, grubEscape(img.KernelParams))
	if err := os.WriteFile(filepath.Join(work, "grub.cfg"), []byte(cfg), 0o644); err != nil {
		return "", err
	}

	out := filepath.Join(outDir, fmt.Sprintf("NOSaic-%s-%s.bin", img.Version, img.Board))
	f, err := os.Create(out)
	if err != nil {
		return "", err
	}
	defer f.Close()
	head := fmt.Sprintf(installerONIEGRUB, img.Version, img.Board, img.Arch,
		boot.Size/2, slot.Size/2, data.Size/2, img.Console)
	if _, err := f.WriteString(head); err != nil {
		return "", err
	}
	cmd := exec.Command("tar", "-cf", "-", "-C", work,
		"boot.img.gz", "slot-a.sqsh", "data.img.gz", "vmlinuz", "initrd.img", "grub.cfg")
	cmd.Stdout = f
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("appending the payload: %w", err)
	}
	if err := os.Chmod(out, 0o755); err != nil {
		return "", err
	}
	return out, nil
}

type gptPart struct {
	Start int64  `json:"start"`
	Size  int64  `json:"size"`
	Name  string `json:"name"`
}

// namedPartitions reads a disk image's GPT, keyed by partition name.
func namedPartitions(disk string) (map[string]gptPart, error) {
	b, err := exec.Command("sfdisk", "--json", disk).Output()
	if err != nil {
		return nil, fmt.Errorf("reading the partition table of %s: %w", disk, err)
	}
	var doc struct {
		PartitionTable struct {
			Partitions []gptPart `json:"partitions"`
		} `json:"partitiontable"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	out := map[string]gptPart{}
	for _, p := range doc.PartitionTable.Partitions {
		if p.Name != "" {
			out[p.Name] = p
		}
	}
	return out, nil
}

// extractGz copies one partition out of a disk image, gzipped.
func extractGz(disk string, p gptPart, dst string) error {
	in, err := os.Open(disk)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".raw"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, io.NewSectionReader(in, p.Start*512, p.Size*512)); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	defer os.Remove(tmp)
	_, err = gzipFile(tmp, dst)
	return err
}

// grubEscape protects a kernel command line inside a grub.cfg linux line.
//
// ⚠ GRUB EXPANDS $ THERE. The DMA reservation is written memmap=64M$<addr>,
// and unescaped GRUB reads $<addr> as a variable, finds nothing, and hands
// the kernel memmap=64M -- a setting that is wrong rather than missing. A
// backslash makes GRUB pass the next character through as itself.
func grubEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `$`, `\$`).Replace(s)
}
