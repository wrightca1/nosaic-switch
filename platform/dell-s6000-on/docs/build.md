# Building an image for the Dell S6000-ON

The general build is in [docs/BUILDING.md](../../../docs/BUILDING.md). This
page covers only what differs here.

## The short version

```sh
make toolchain ARCH=x86_64
make packages ARCH=x86_64 PROFILE=minimal
make pkg PKG=linux ARCH=x86_64
make image   BOARD=dell-s6000-on      # the installer
make netboot BOARD=dell-s6000-on      # the RAM-only kexec bundle
```

These produce:

- `out/images/dell-s6000-on/NOSaic-<version>-dell-s6000-on.bin`: the x86
  ONIE installer (`boot: onie-grub`).
- `out/images/dell-s6000-on/netboot/`: `vmlinuz`, a RAM-boot `initrd.img`,
  `cmdline`, and `NOSaic-<version>-dell-s6000-on-netboot.bin`, which ONIE runs
  and which kexecs into NOSaic without touching the disk.

## What this board needs that the generic build does not

- **The `onie-grub` backend.** It is x86 ONIE, not the U-Boot-shaped
  `onie-sfx`, and it installs beside ONIE the way SONiC does.
- **Kernel drivers** for the platform, which are not all in the x86_64
  fragment yet: `i2c-ismt`, `lpc_sch`, `gpio-sch`, `i2c-mux-gpio`, `nvram`,
  `at24`, `jc42`, `emc1403`, `lm75`, `max6620`, `ltc4215`, and `pmbus` for the
  DPS-460 supplies. None are needed to boot, only for the platform HAL.
- **No firmware blobs.** The ASIC is driven by `nosd-td2` over OpenBCM, and
  there are no external PHYs or retimers to load firmware into.

## Profile

`minimal` (s6), like every board that boots NOSaic today. The disk is a 16 GB
SSD, so the layout (64 MiB boot, 2 × 1 GiB slots, 1 GiB data) is not tight.

## Verifying before you install

All of this runs off the switch, under QEMU with SeaBIOS standing in for
Dell's legacy BIOS, and a real ONIE built as `kvm_x86_64` with
`UEFI_ENABLE=no`. Build that ISO from the
[ONIE repository](https://github.com/opencomputeproject/onie) with
`make MACHINE=kvm_x86_64 UEFI_ENABLE=no all recovery-iso`, then:

```sh
platform/dell-s6000-on/tools/rehearse.sh ramboot
ONIE_ISO=<path>/onie-recovery-x86_64-kvm_x86_64-r0.iso \
    platform/dell-s6000-on/tools/rehearse.sh onie netboot install
```

Logs and VM disks land in `out/rehearsal/dell-s6000-on/`.

| Test | Proves |
|---|---|
| ramboot | the kernel and RAM initramfs boot to userspace and pass the self-test |
| netboot | ONIE runs the netboot `.bin`, kexecs into NOSaic, and the disk is byte-for-byte unchanged |
| install | the installer adds four named partitions beside ONIE, GRUB boots NOSaic from slot A (found by name), ONIE's partitions do not move, a live `upgrade install --disk` writes only `nosaic-slot-b`, and ONIE still boots from our GRUB menu |

What QEMU cannot stand in for is the BCM56850, the CPLDs and i2c tree, the
S1220's iSMT SMBus, and Dell's BIOS. Those are the bring-up.
