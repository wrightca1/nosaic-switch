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
| Disk | CFast card behind a Marvell 88SE9170 (AHCI) at `02:00.0`, so `/dev/sda`; ONIE finds it by that PCI path |
| Management NIC | Intel 82574L (`e1000e`) |
| SMBus | SCH legacy SMBus on the LPC bridge `00:1f.0` (`i2c-isch`, via `lpc_sch`): the mux tree. iSMT `00:13.1` (`i2c-ismt`): the PSUs. iSMT `00:13.0`: unused |
| GPIO | S1200 PCU GPIO through `lpc_sch` / `gpio-sch` |
| Management | three CPLDs on i2c: system `0x31`, master `0x32`, slave `0x33` |

## Read off a unit

From gmrproxsw02 running SONiC 202012 (read-only commands):

| | |
|---|---|
| ASIC | `01:00.0` BCM56850_A2 (rev 03), BAR0 `0xff600000` 256 KB, INTx IRQ 21, MSI capable; PCIe Gen2 x2, max payload 128 |
| Usable RAM below 4 GB | `0x00100000`-`0xbed0cfff` (e820); SONiC's kernel BDE put its 32 MB DMA pool at `0xb8c00000` |
| Disk | CFast 3IE, 16 GB GPT: `GRUB-BOOT` 2 MB, `ONIE-BOOT` 128 MB, `PLATFORM-DIAG` 300 MB (Dell diagnostics), `SONiC-OS` 14.5 GB |
| GPIO | one chip, `sch_gpio.3168`, 30 lines; the mux uses lines 1, 2 and 10 |
| CPLDs | system `0xa`, master `0xc`, slave `0xa` |
| PSUs | 2 x Dell `02RH8M` (DPS-460), status register `0x22` for both: present, not failed, power good |
| Idle thermals | board tmp75s 29-34 C, CPU 28-32 C, DIMM 31 C; fans 10031 rpm (52%), PSU fans ~15000 rpm |
| Ports under SONiC | pause off, **software** linkscan, autoneg off, 40G interface **XGMII**, max frame 9122 |
| LED processors | running: `bcmcmd "led status"` maps each xe port to a processor and LED index |

## Block diagram

```
 S1220 ─ PCIe ─ BCM56850 ─ SerDes ─ 32 × QSFP+
   │
   ├─ iSMT 00:13.1 (SONiC's i2c-1): PSU FRUs 0x50/0x51, DPS-460 PMBus 0x58/0x59
   └─ SCH SMBus 00:1f.0 (SONiC's i2c-2) ─ 74CBTLV3253 1:4 mux (GPIO 1,2; reset GPIO 10)
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

Not shipped, per NOSaic's rule on vendor-derived per-port data. `tools/mkconf.sh`
turns any S6000 `td2-s6000-32x40G.config.bcm` -- from a unit running SONiC or
from sonic-buildimage -- into `portmap.conf`, `polarity.conf` and `serdes.conf`.
It rewrites SONiC's `_xeK` SerDes keys to logical port numbers, checks every
preemphasis value carries the tap-force bit, and refuses rather than writing
half a file. The data describes the board's copper, not the unit, so one run
serves every S6000.

Cages are numbered 1-32 in front-panel order (cage N is SONiC's
`fortyGigE0/<4(N-1)>`; checked against `port_config.ini`). With no breakout,
logical port N is cage N and the interfaces are `et1`-`et32`.

**4x10G breakout.** `mkconf.sh --breakout 29,30` gives each listed cage four
logical numbers (renumbering the cages after it), writes
`nosaic_portmode_<base>=4x10g` to `portmode.conf`, and names the lanes
`et29_1`-`et29_4`. The datapath's portmode then maps the four lanes as 10G
ports at boot. Per-lane data follows Dell's own breakout configs, checked
against the Q24S32 SKU for the same cage: lane 1 keeps the 40G port's lane
maps, polarity masks and serdes values; lanes 2-4 get only one bit of the
polarity mask each. Limits enforced: 52 front-panel ports per pipeline (cages
1-16, 17-32) and NOSaic's 64 interfaces. Switching a cage between modes is
re-running the generator and restarting `nosd`. The unit read so far runs
cages 29 and 30 as 4x10G under SONiC (dynamic port breakout; 38 ports).

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

## Port LEDs

Driven by the Trident II's own two LED processors (CMICm `CMIC_LEDUP0/1`),
not a CPLD. Dell's `led_proc_init.soc` loads the same 132-byte program into
both and a remap from LED slot to port (52 of 64 slots used per processor):
LED on at link, blinking on activity. `tools/mkledproc.sh` turns it into
`ledproc.conf`, and `datapath/td2/ledproc.c` loads and starts both processors
after `bcm_init`. The program reads link and activity from the hardware scan
chain, so no linkscan callback is needed. Unverified on hardware: if a cage
with link stays dark, read `DATA_RAM[2*idx+1]` bit 0 for its LED index.

## Platform HAL

`internal/platformhal/s6000`, driver `dell-s6000`, all in userspace:

- the GPIO mux through the gpio character device (lines requested and
  released per transaction, so the thermal service and the CLI can share it),
  under a lock file, with Dell's reset-and-retry on a failed transfer;
- the CPLDs, sensors, fan controllers and QSFP EEPROMs through `/dev/i2c-N`,
  each decoded as its Linux driver decodes it (lm75, emc1403, jc42, max6620);
- the two buses found by PCI function, stated in `board.yml` from a running
  unit's sysfs, and checked at open by reading the system CPLD.

It implements `HAL`, `Cooling` (max6620 RPM mode, 19000 RPM = 100 %),
`Optics` (the CPLD cage select, then the cage's channel, under one lock),
`thermal.Lamps` (system, tray and front fan LEDs), `PowerCycle`, and
`ReleaseSwitchChip` as Dell's cage init: low power off, one-second reset.

⚠ No kernel hwmon driver may be bound behind the mux: it would read whatever
channel this driver left selected.

⚠ The fan-to-tray pairing is not settled: Dell's fancontrol.sh, fan.py and
set-fan-speed disagree. The driver follows fancontrol.sh.

## Quirks

- **Reboot is a full reset, never a warm one.** Dell's ONIE replaces
  `reboot` with `i2cset -y 0 0x31 1 0xfd`, a CPLD power cycle its comment says
  the ASIC needs. Dell's SONiC writes the reboot reason (0x0e) to CMOS byte
  0x49 and then does a full cold reset through port 0xCF9 (0x0e). NOSaic's
  shutdown runs `nosaic platform power-cycle`, and falls back to the kernel's
  reboot, which `reboot=p` makes the same 0xCF9 cold reset SONiC uses.
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
