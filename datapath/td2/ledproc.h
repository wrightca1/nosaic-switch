/* SPDX-License-Identifier: Apache-2.0 */
#ifndef NOSAIC_TD2_LEDPROC_H
#define NOSAIC_TD2_LEDPROC_H

/* Load and start the LED processors from ledproc_* properties, after
 * bcm_init. Returns how many processors were started; 0 when the board
 * declares none. */
int nosaic_ledproc_start(int unit);

#endif
