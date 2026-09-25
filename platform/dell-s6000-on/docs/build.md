# Building an image for the Dell S6000-ON

The general build is in [docs/BUILDING.md](../../../docs/BUILDING.md). This
page covers only what differs here.

## The short version

```sh
make toolchain ARCH=x86_64
make packages ARCH=x86_64 PROFILE=minimal
make pkg PKG=linux ARCH=x86_64
make pkg PKG=openbcm ARCH=x86_64      # 25+ GB of build tree; see below
make pkg PKG=nosd-td2 ARCH=x86_64

# the per-port tables, into the image (never committed)
platform/dell-s6000-on/tools/mkconf.sh <td2-s6000-32x40G.config.bcm> platform/dell-s6000-on/config

make netboot BOARD=dell-s6000-on      # the RAM-only kexec bundle
make image   BOARD=dell-s6000-on      # the installer -- build it last
```

`make netboot` and `make image` share `out/images/dell-s6000-on/`; build the
installer last so the files beside it are the installer's.

OpenBCM's build tree passes 25 GB. A package already built from the same
`recipes/openbcm` for x86_64 can be dropped into `out/packages/` instead.

These produce:

- `out/images/dell-s6000-on/NOSaic-<version>-dell-s6000-on.bin`: the x86
  ONIE installer (`boot: onie-grub`).
- `out/images/dell-s6000-on/netboot/`: `vmlinuz`, a RAM-boot `initrd.img`,
  `cmdline`, and `NOSaic-<version>-dell-s6000-on-netboot.bin`, which ONIE runs
  and which kexecs into NOSaic without touching the disk.

## What this board needs that the generic build does not

- **The `onie-grub` backend.** It is x86 ONIE, not the U-Boot-shaped
  `onie-sfx`, and it installs beside ONIE the way SONiC does.
- **Kernel drivers**, now in `recipes/linux/config/x86_64.fragment`:
  `i2c-isch` with `lpc_sch` (the SCH SMBus the whole mux tree hangs off --
  without it the HAL has no CPLD, sensor, fan or QSFP), `i2c-ismt` (the
  PSUs), and `gpio-sch` (the mux lines). The HAL reads every sensor itself;
  the hwmon modules for them are built but must not be loaded.
- **No firmware blobs.** The ASIC is driven by `nosd-td2` over OpenBCM, and
  there are no external PHYs or retimers to load firmware into.

## Profile

`minimal` (s6), like every board that boots NOSaic today. The layout (64 MiB
boot, 2 × 1 GiB slots, 1 GiB data) needs about 3.1 GiB beside ONIE on the
internal CFast card, whose size is not recorded yet.

## Verifying before you install

All of this runs off the switch, under QEMU with SeaBIOS standing in for
Dell's legacy BIOS, and a real ONIE built as `kvm_x86_64` with
`UEFI_ENABLE=no`. Build that ISO from the
[ONIE repository](https://github.com/opencomputeproject/onie) with
`make MACHINE=kvm_x86_64 UEFI_ENABLE=no SECURE_BOOT_ENABLE=no SECURE_BOOT_EXT=no SECURE_GRUB=no all recovery-iso`
(the S6000 has neither UEFI nor Secure Boot), then:

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
