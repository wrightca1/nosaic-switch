# Dell S6000-ON — hardware reference

Recovered from the GPL platform driver and scripts Dell contributed to SONiC
(`sonic-buildimage`, branch `202012`,
`platform/broadcom/sonic-platform-modules-dell/s6000`) and from a SONiC 202012
image for this board. **None of it has been observed under NOSaic yet.**

## At a glance

| | |
|---|---|
| CPU | Intel Atom S1220 (Centerton), x86_64, legacy BIOS |
| ASIC | Broadcom BCM56850 Trident II, PCI `14e4:b850` at `01:00.0` |
| Front panel | 32 × QSFP+ 40G, each breakable to 4 × 10G |
| PHYs / retimers | **none**: every cage is on the ASIC's own SerDes |
| Loader | ONIE on x86 GRUB, on the same disk as the NOS |
| SMBus | 2 × Intel iSMT (`8086:0c59`, `8086:0c5a`), driver `i2c-ismt` |
| GPIO | S1200 PCU GPIO through `lpc_sch` / `gpio-sch` |
| Management | three CPLDs on i2c: system `0x31`, master `0x32`, slave `0x33` |

## Block diagram

```
 S1220 ─ PCIe ─ BCM56850 ─ SerDes ─ 32 × QSFP+
   │
   ├─ iSMT ─ i2c (SONiC's i2c-1): PSU FRUs 0x50/0x51, DPS-460 PMBus 0x58/0x59
   └─ iSMT ─ i2c (SONiC's i2c-2) ─ 74CBTLV3253 1:4 mux (GPIO 1,2; reset GPIO 10)
        ch0: CPLDs 0x31-0x33, jc42 0x18, emc1403 0x4d, SPD 0x50, sys EEPROM 0x53
        ch1: max6620 0x29/0x2a, ltc4215 0x40/0x42, tmp75 0x4c/0x4d/0x4e,
             fan-tray EEPROMs 0x51-0x53
        ch2: QSFP 0-15  (selected by the slave CPLD)
        ch3: QSFP 16-31 (selected by the master CPLD)
```

## Boot chain

BIOS → GRUB (MBR + GRUB-BOOT) → menu: NOSaic or ONIE. NOSaic's GRUB modules and
`grub.cfg` live on `nosaic-boot` along with the kernel and initramfs, and
ONIE's entries are appended from ONIE's own `50_onie_grub`. SONiC boots with
`processor.max_cstate=1 intel_idle.max_cstate=0`, and so does this board.

## Port map

Not shipped, per CONTRIBUTING: the port map, lane maps, polarity and SerDes
preemphasis/driver-current are vendor-derived per-port data. Generate them
from the switch's own SONiC `td2-s6000-32x40G.config.bcm` with the board
tools, the way the other td2 boards do.

## Register and memory regions

The CPLDs are SMBus devices, not memory-mapped.

| CPLD | reg | meaning |
|---|---|---|
| system 0x31 | 0x00 [3:0] | version |
| system 0x31 | 0x01 | write `0xfd`: **hard power-cycle reset** |
| master 0x32 | 0x01 [3:0] | version |
| master 0x32 | 0x03 | PSU: b7 PSU0 absent, b6 PSU0 bad, b3 PSU1 absent, b2 PSU1 bad (active low) |
| master 0x32 | 0x07 | LEDs: [6:5] system, [4:3] locator, [2:1] power, [0] master |
| master 0x32 | 0x08 | [7:6] fan trays 0-1 present; tray 0/1/2 LEDs at [1:0]/[3:2]/[5:4] |
| master 0x32 | 0x09 | [0] fan tray 2 present; [4:3] front fan LED |
| slave 0x33 | 0x0a [3:0] | version |

QSFP control is 32-bit vectors. Ports 0-15 are in the slave CPLD, ports 16-31
in the master CPLD, low byte first:

| signal | slave 0x33 | master 0x32 | sense |
|---|---|---|---|
| ModSel (also the i2c select) | 0x00/0x01 | 0x0a/0x0b | active low, one-hot |
| LPMode | 0x02/0x03 | 0x0c/0x0d | 1 = low power |
| ModPrs | 0x04/0x05 | 0x0e/0x0f | active low |
| Reset | 0x06/0x07 | 0x10/0x11 | active low |

The reboot reason is CMOS NVRAM byte `0x49`: `0x0e` cold, `0x06` warm, `0x07`
thermal.

## Datapath

`nosd-td2`, the same datapath as the 7050TX-64 and N3172TQ. This is its first
BCM56850 (the others are 56855 and 56854). There are no external PHYs, so the
PHY/MDIO bring-up those boards need does not apply. The DMA reservation
(`memmap=`) has to be read off this board's e820 before nosd can run.

## Platform HAL

Not written. The generic `I2CMap` does not fit as-is: the QSFP mux is the
CPLDs rather than a pca954x, and the whole tree sits behind a GPIO mux. The
closest existing driver is `n3172tq`.

## Quirks

- **Reboot through the CPLD.** SONiC and Dell's ONIE both replace `reboot`
  with `i2cset -y 0 0x31 1 0xfd`. The ASIC needs a hard power cycle to come
  back correctly.
- **The i2c mux wedges.** Dell's driver pulses GPIO 10 and retries once
  whenever an SMBus transfer fails.
- **QSFP reset at init.** SONiC takes all cages out of low power and pulses
  reset for 1 s at boot.
- **Fans default to 15000 RPM** at init, before any control loop runs.

## Reverse engineering

Sources: `dell_s6000_platform.c` (1399 lines, GPL), `s6000_platform.sh`,
`sonic_platform/*.py`, `sensors.conf`, `installer.conf` and `pcie.yaml`, all from
sonic-buildimage `202012`. The SONiC image is build 63292
(`202012.63292-a584de90f`).
