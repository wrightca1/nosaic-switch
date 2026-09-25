# Installing NOSaic on the Dell S6000-ON

⚠ **Nothing here has run on an S6000 yet.** Every step has been rehearsed
under QEMU against a real ONIE (kvm_x86_64, legacy BIOS). See "Verifying
before you install" in [build.md](build.md). Treat the first run on hardware as
bring-up.

## Before you start

- **Netbooting** writes nothing to the switch. Try it first.
- **Installing** adds four partitions (`nosaic-boot`, `nosaic-slot-a`,
  `nosaic-slot-b`, `nosaic-data`) after ONIE's on the same disk and replaces
  the MBR's GRUB. ONIE's own partitions are not touched, and ONIE stays on
  the GRUB menu.
- If another NOS (SONiC, FTOS/OS9 via ONIE) fills the disk, the installer
  stops and says so. Remove that NOS with ONIE's **Uninstall OS** entry
  first. That is not reversible without that NOS's own installer, so keep a
  copy of it.

## Console

The RJ-45 console port on the front panel, 8N1, no flow control. Use the
baud rate ONIE itself uses (`cat /proc/cmdline` in ONIE's rescue shell). The
NOSaic installer and the netboot image both read it from there, so nothing
has to be guessed.

## 1. Netboot first (RAM only)

Build the bundle (`make netboot BOARD=dell-s6000-on`) and serve
`out/images/dell-s6000-on/netboot/` over HTTP. Boot the switch into ONIE
(**ONIE: Rescue** or **ONIE: Install OS**), then:

```
ONIE:/ # onie-stop
ONIE:/ # which kexec
ONIE:/ # onie-nos-install http://<server>/<path>/NOSaic-<version>-dell-s6000-on-netboot.bin
```

Expect `kexec into NOSaic`, then the kernel, `NOSAIC-INITRAMFS starting`, and
a login prompt. Power-cycle the switch to get back to ONIE.

If `which kexec` prints nothing, this ONIE predates kexec support. Boot
`vmlinuz` and `initrd.img` from ONIE's GRUB instead, as the bundle's README
describes.

## 2. Install

Build the installer (`make image BOARD=dell-s6000-on`), serve
`out/images/dell-s6000-on/NOSaic-<version>-dell-s6000-on.bin`, and from ONIE:

```
ONIE:/ # onie-stop
ONIE:/ # onie-nos-install http://<server>/<path>/NOSaic-<version>-dell-s6000-on.bin
```

You should see, in order:

```
ONIE is on /dev/sda2; installing beside it on /dev/sda
nosaic-boot is partition N ... nosaic-data is partition N+3
writing nosaic-boot / nosaic-slot-a / clearing nosaic-slot-b / writing nosaic-data
Installation finished. No error reported.     (grub-install)
console ttyS0,<baud>
NOSaic installed.
```

ONIE then reboots the switch.

## First boot

The GRUB menu offers **NOSaic <version>** (the default) and ONIE's entries.
NOSaic boots from slot A. Log in as `root` on the console. There is no password
until you set one with `passwd`.

## Upgrading

On the running switch, name the whole disk. The slot is found on it by
partition name, and the boot pointer on the mounted boot partition is updated:

```
nosaic upgrade install /tmp/new.sqsh --disk /dev/sda
reboot
nosaic upgrade commit        # once the new slot is running and healthy
```

An upgrade does not replace the kernel yet. It stays on `nosaic-boot`.

## Going back

- **To ONIE:** choose ONIE from the GRUB menu. Install or uninstall from there.
- **To another NOS:** ONIE's **Install OS** with that NOS's installer. Its
  installer will replace GRUB and may remove NOSaic's partitions.

## When it does not work

- **`error: no ONIE-BOOT partition found`**: this is not ONIE, or ONIE lives
  on a different disk.
- **`error: this switch booted ONIE through UEFI`**: this backend only supports
  legacy BIOS so far. The S6000 is BIOS, so if you see this, say so.
- **`NOSaic needs N MiB free`**: another NOS holds the disk. See "Before you start".
- **A warm reboot leaves the ports dead**: the ASIC needs the CPLD power reset.
  Power-cycle the switch. See [hardware.md](hardware.md) Quirks.
