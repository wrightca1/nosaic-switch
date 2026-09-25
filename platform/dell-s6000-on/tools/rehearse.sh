#!/bin/sh
# Off-switch rehearsal for the Dell S6000-ON, under QEMU in the builder
# container. Run from the repository root:
#
#   platform/dell-s6000-on/tools/rehearse.sh ramboot
#   ONIE_ISO=/path/to/onie-recovery-x86_64-kvm_x86_64-r0.iso \
#       platform/dell-s6000-on/tools/rehearse.sh onie netboot install
#
# Needs `make image` and `make netboot` for this board first. The ONIE ISO is
# ONIE's kvm_x86_64 target built with UEFI_ENABLE=no, to match the S6000's
# legacy BIOS; see docs/build.md.
set -e
ROOT=$(cd "$(dirname "$0")/../../.." && pwd)
IMG=out/images/dell-s6000-on
ISO=${ONIE_ISO:-}
MOUNT_ISO=
if [ -n "$ISO" ]; then
    MOUNT_ISO="-v $(cd "$(dirname "$ISO")" && pwd)/$(basename "$ISO"):/onie.iso:ro"
fi
cd "$ROOT"
exec docker run --rm --network host -v "$ROOT:/src" -w /src $MOUNT_ISO nosaic/builder:0.1 \
    python3 -u platform/dell-s6000-on/tools/rehearse.py "$@" \
        --netboot "$IMG/netboot" \
        --installer "$(ls $IMG/NOSaic-*-dell-s6000-on.bin 2>/dev/null | head -1)" \
        --squashfs "$(ls $IMG/*.sqsh $IMG/rootfs.squashfs 2>/dev/null | head -1)" \
        --iso /onie.iso \
        --out out/rehearsal/dell-s6000-on
