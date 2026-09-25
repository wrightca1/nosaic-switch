# Dell S6000-ON — what is left

From the SONiC image's own bring-up, compared with NOSaic's td2 datapath
(the evidence is kept outside this tree with the rest of the reverse
engineering). Nothing here has run on an S6000 yet.

## Stops it working

- **DMA reservation.** `kernel_params` has no `memmap=`; `nosd-td2` refuses to
  guess and will not attach. Needs a free 64 MB of usable RAM below 4 GB from
  a unit's `dmesg | grep e820` (SONiC gives the SDK 32 MB).
- **Link state never reaches Linux.** No linkscan callback sets the taps'
  carrier, so a dead port's routes stay in the kernel and the chip until the
  routing protocol's own timers expire. Datapath-wide, not S6000-specific.
- **Some traffic for the switch never reaches the CPU.** Only the taps' own
  addresses are trapped: BGP to a loopback is dropped in hardware, a neighbour
  the kernel has not resolved yet is black-holed, and TTL-expired / MTU-exceeded
  packets are dropped silently (traceroute, PMTU). Datapath-wide.

## Needs doing before it carries anything real

- **CPU protection.** No meters or per-queue CPU mapping; every CPU-bound packet
  shares one queue, capped only by a 20 kpps software limit after DMA. SONiC
  keeps BGP/LACP/LLDP on queue 4 and meters ARP/ND at 600 pps, IP2ME at 6000.
  An ARP storm here competes with BGP keepalives on a 2-core Atom.
- **Interrupts.** `polled_irq_mode=1`, as on both td2 siblings. Polling cost
  the 7050SX2 two cores and ~19 packets/s to the CPU; the S1220 has two cores.
  `asic_pci` is set so the uio binding exists; switch to interrupts with the
  7050SX2's recipe (its asic.conf: tdma/tslam/schan and DMA timeouts) once
  ports pass traffic polled.

## Worth doing

- ECMP hash: NOSaic writes the older hash; SAI uses the enhanced 5-tuple hash
  with a seed.
- MTU: taps are 1500 / frame max 1522; SONiC runs 9100.
- Per-cable SerDes: SONiC re-applies preemphasis for 3 m 40G DAC
  (`media_settings.json`); NOSaic uses config.bcm's defaults for everything.
- SER / parity events: nothing registered to report them.
- 4x10G breakout: needs a renumbered map; the SONiC image carries the data.
- A port whose PHY init fails is never retried.

## Done without the switch (verify on the first unit)

- Platform HAL on the SCH SMBus mux tree, the CPLD power-cycle reboot, QSFP
  control, fans, sensors, PSUs, LEDs.
- ASIC LED processors (`ledproc.conf`).
- Pause off, software linkscan and software RX-LOS (`asic.conf`).
- Retimer probing only where the SDK bound a BCM84328.
- Fan-to-tray pairing: Dell's files disagree; pull a tray and watch
  `nosaic platform status`.
