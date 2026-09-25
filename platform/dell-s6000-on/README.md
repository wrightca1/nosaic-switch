# Dell S6000-ON

Broadcom BCM56850 Trident II, 32 × QSFP+ 40G, Intel Atom S1220, x86 ONIE with
legacy BIOS GRUB.

**Status: planned.** Nothing has run on the hardware yet. The boot, install
and A/B paths are rehearsed under QEMU against a real ONIE
(`tools/rehearse.sh`).

| Page | For |
|---|---|
| [docs/install.md](docs/install.md) | netbooting (RAM only) and installing beside ONIE |
| [docs/build.md](docs/build.md) | building the images and running the off-switch rehearsal |
| [docs/hardware.md](docs/hardware.md) | I2C tree, CPLD registers, quirks |

This board introduces the `onie-grub` boot backend (`internal/boot/oniegrub.go`),
for ONIE on x86. ONIE shares the disk there, so NOSaic's partitions are added
beside ONIE's and found by GPT name, as SONiC installs.
