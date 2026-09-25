/* SPDX-License-Identifier: Apache-2.0 */
/*
 * The chip's own LED processors (CMIC_LEDUP0/1), loaded from ledproc.conf.
 *
 * On a Trident II board whose port LEDs are driven by the ASIC rather than by
 * a board controller -- the Dell S6000-ON -- nothing lights until a program is
 * loaded into the LED processors and they are started. soc init sets the scan
 * chains up and leaves them stopped (_soc_td2_ledup_init). SONiC does the rest
 * with `bcmcmd rcload led_proc_init.soc`; this does the same through the SDK's
 * public calls, from a file tools/mkledproc.sh generates from that .soc.
 *
 * The same thing `led N stop / prog / start` and the PORT_ORDER_REMAP modregs
 * do, in that order. `led N auto on` is not reproduced: it only registers a
 * linkscan callback that writes DATA_RAM[0xA0..], and the S6000's program
 * never reads there -- it takes link and activity from the hardware scan
 * chain. A board whose program does read that area needs the callback.
 *
 * A board with no ledproc.conf gets no LEDUP writes at all. Failures are
 * reported and never fatal: dark LEDs are not a reason to stop forwarding.
 */
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <sal/types.h>
#include <bcm/error.h>
#include <bcm/switch.h>
#include "props.h"
#include "ledproc.h"

/* Declared rather than taken from <soc/drv.h>, for the reason sdk.c gives. */
extern int    soc_pci_getreg(int unit, uint32 addr, uint32 *data);
extern uint32 soc_pci_write_helper(int unit, uint32 addr, uint32 data);

#define LEDUP_STRIDE   0x1000u  /* CMIC_LEDUP1_* - CMIC_LEDUP0_* (cmicm.h)   */
#define LEDUP0_STATUS  0x20004u /* CMIC_LEDUP0_STATUS_OFFSET: RUN=bit8, PC=7:0 */
#define LEDUP0_REMAP   0x20010u /* CMIC_LEDUP0_PORT_ORDER_REMAP_0_3_OFFSET    */

static const char *get(int unit, int uc, const char *what, int n)
{
	char k[64];

	snprintf(k, sizeof(k), n < 0 ? "ledproc_%d_%s" : "ledproc_%d_%s_%d", uc, what, n);
	return nosaic_props_get_unit(k, unit);
}

static int load_prog(int unit, int uc, uint8 prog[256])
{
	const char *v = get(unit, uc, "prog_len", -1);
	int len = v ? atoi(v) : 0, got = 0;

	if (v == NULL)
		return 0;                         /* this processor is not configured */
	memset(prog, 0, 256);
	while (got < len && (v = get(unit, uc, "prog", got)) != NULL) {
		for (; v[0] && v[1] && got < 256; v += 2) {
			char b[3] = { v[0], v[1], 0 }, *end;

			prog[got++] = (uint8)strtoul(b, &end, 16);
			if (*end) return -1;
		}
		if (*v) return -1;                /* odd digit count or overlong */
	}
	return (len > 0 && len <= 256 && got == len) ? len : -1;
}

int nosaic_ledproc_start(int unit)
{
	static const uint8 zeros[128];
	uint8 prog[256];
	const char *v;
	uint32 st = 0;
	int uc, slot, len, rv, started = 0;

	for (uc = 0; uc < 2; uc++) {
		if ((len = load_prog(unit, uc, prog)) == 0)
			continue;
		if (len < 0) {
			fprintf(stderr, "ledproc: ledproc_%d_prog_* is malformed; LED processor %d left alone\n", uc, uc);
			continue;
		}
		/* led N stop; led N prog (256 bytes, zero padded, as ledproc_load does) */
		if ((rv = bcm_switch_led_fw_start_set(unit, uc, 0)) < 0 ||
		    (rv = bcm_switch_led_fw_load(unit, uc, prog, 256)) < 0 ||
		    (rv = bcm_switch_led_control_data_write(unit, uc, 0x80, zeros, 128)) < 0) {
			fprintf(stderr, "ledproc: processor %d: %s\n", uc, bcm_errmsg(rv));
			continue;
		}
		/* modreg CMIC_LEDUPn_PORT_ORDER_REMAP_*: read-modify-write, 6 bits a slot */
		for (slot = 0; slot < 64; slot++) {
			v = get(unit, uc, "remap", slot);
			uint32 addr = LEDUP0_REMAP + uc * LEDUP_STRIDE + 4 * (slot / 4), r;
			unsigned long x = v ? strtoul(v, NULL, 0) : 0;

			if (v == NULL) continue;
			if (x > 63) { fprintf(stderr, "ledproc: ledproc_%d_remap_%d=%s > 63\n", uc, slot, v); continue; }
			soc_pci_getreg(unit, addr, &r);
			r = (r & ~(0x3fu << (6 * (slot % 4)))) | ((uint32)x << (6 * (slot % 4)));
			soc_pci_write_helper(unit, addr, r);
		}
		/* led N auto on: deliberately not reproduced -- see notes. */
		v = get(unit, uc, "start", -1);            /* led N start */
		if (v != NULL && atoi(v) == 1 &&
		    (rv = bcm_switch_led_fw_start_set(unit, uc, 1)) < 0) {
			fprintf(stderr, "ledproc: start %d: %s\n", uc, bcm_errmsg(rv));
			continue;
		}
		soc_pci_getreg(unit, LEDUP0_STATUS + uc * LEDUP_STRIDE, &st);
		printf("ledproc: processor %d: %d-byte program, %s (PC 0x%02x)\n", uc, len,
		       (st & 0x100) ? "running" : "stopped", (unsigned)(st & 0xff));
		started++;
	}
	return started;
}
