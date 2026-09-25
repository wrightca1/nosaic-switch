#!/usr/bin/env python3
"""Off-switch rehearsal of NOSaic on the Dell S6000-ON, under QEMU.

Everything the switch will do, done first on a legacy-BIOS (SeaBIOS) x86 VM
running a real ONIE -- so that the first time the S6000 sees an image, the
image has already booted, kexec'd, installed and upgraded somewhere else.

  ramboot  boot the netboot kernel + RAM initramfs directly (no ONIE)
  onie     make a VM disk with ONIE embedded on it, as a switch has
  netboot  from that ONIE: onie-nos-install the kexec netboot .bin
  install  from that ONIE: onie-nos-install the onie-grub installer, boot
           NOSaic from disk, check ONIE's partitions survived, run an A/B
           upgrade into slot b, and boot back into ONIE from our GRUB menu

What QEMU cannot stand in for -- the BCM56850, the CPLDs, the i2c tree, the
S1220's iSMT controllers, Dell's BIOS -- is left for the switch.

Runs inside the nosaic builder container (qemu-system-x86_64, python3,
sfdisk). No KVM is assumed; everything is sized for TCG.
"""
import argparse
import hashlib
import http.server
import json
import os
import re
import select
import socketserver
import subprocess
import sys
import threading
import time

HOST = "10.0.2.2"  # the host, as QEMU user networking shows it to the guest
PORT = 8000


class Fail(Exception):
    pass


class VM:
    """A QEMU with its serial console on stdio, driven like an expect script."""

    def __init__(self, args, log):
        self.log = open(log, "wb")
        self.buf = b""
        self.pos = 0
        print("  $ " + " ".join(args))
        self.p = subprocess.Popen(args, stdin=subprocess.PIPE,
                                  stdout=subprocess.PIPE, stderr=subprocess.STDOUT)

    def _read(self, timeout):
        r, _, _ = select.select([self.p.stdout], [], [], timeout)
        if not r:
            return False
        chunk = os.read(self.p.stdout.fileno(), 65536)
        if not chunk:
            raise Fail("QEMU exited")
        self.buf += chunk
        self.log.write(chunk)
        self.log.flush()
        return True

    def expect(self, patterns, timeout, fail=(r"Kernel panic",)):
        """Wait for any of patterns after the last match; return its index."""
        if isinstance(patterns, str):
            patterns = [patterns]
        deadline = time.time() + timeout
        while True:
            text = self.buf[self.pos:].decode("utf-8", "replace")
            for f in fail:
                if re.search(f, text):
                    raise Fail("saw %r\n%s" % (f, self.tail()))
            for i, pat in enumerate(patterns):
                m = re.search(pat, text)
                if m:
                    self.pos += len(text[:m.end()].encode("utf-8", "replace"))
                    return i
            left = deadline - time.time()
            if left <= 0:
                raise Fail("timed out after %ds waiting for %s\n%s" % (timeout, patterns, self.tail()))
            try:
                self._read(min(left, 1.0))
            except Fail:
                if self.p.poll() is not None:
                    raise Fail("QEMU exited while waiting for %s\n%s" % (patterns, self.tail()))
                raise

    def send(self, s):
        self.p.stdin.write(s.encode() if isinstance(s, str) else s)
        self.p.stdin.flush()

    def line(self, s):
        # Slowly: a TCG guest's UART drops characters sent in one burst.
        for ch in s + "\r":
            self.send(ch)
            time.sleep(0.02)

    def tail(self, n=40):
        lines = self.buf.decode("utf-8", "replace").splitlines()
        return "\n".join("    | " + l for l in lines[-n:])

    def stop(self):
        if self.p.poll() is None:
            self.p.kill()
            self.p.wait()
        self.log.close()


PROMPT = r"[$#] $"


def login(vm):
    """Log in the way an operator does: admin, no password as shipped."""
    vm.line("admin")
    vm.expect(PROMPT, 120)


def serve(directory):
    """Serve a directory over HTTP for the guest, from inside the container."""
    handler = lambda *a, **k: http.server.SimpleHTTPRequestHandler(*a, directory=directory, **k)

    class Quiet(socketserver.ThreadingMixIn, http.server.HTTPServer):
        daemon_threads = True
        allow_reuse_address = True

    httpd = Quiet(("0.0.0.0", PORT), handler)
    httpd.RequestHandlerClass.log_message = lambda *a: None
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    return httpd


def qemu(disk=None, cdrom=None, mem=2048, extra=()):
    a = ["qemu-system-x86_64", "-nographic", "-m", str(mem), "-smp", "2",
         "-netdev", "user,id=n0", "-device", "e1000,netdev=n0"]
    if cdrom:
        a += ["-cdrom", cdrom, "-boot", "order=cd,once=d"]
    else:
        a += ["-boot", "c"]
    if disk:
        a += ["-drive", "file=%s,format=raw,if=virtio" % disk]
    return a + list(extra)


def partitions(disk):
    out = subprocess.check_output(["sfdisk", "--json", disk])
    return json.loads(out)["partitiontable"]["partitions"]


def part_bytes(disk, p):
    with open(disk, "rb") as f:
        f.seek(p["start"] * 512)
        return f.read(p["size"] * 512)


def sparse_copy(src, dst):
    # VM disks are mostly holes. A plain copy writes every zero and turns a
    # few hundred MiB of real data into the disk's full nominal size.
    subprocess.check_call(["cp", "--sparse=always", src, dst])


def regions(disk):
    """Hash each partition and the gaps around them, keyed by name."""
    parts = partitions(disk)
    out = {"(MBR and GPT)": (0, parts[0]["start"] * 512)}
    for p in parts:
        out[p.get("name") or "part@%d" % p["start"]] = (p["start"] * 512, (p["start"] + p["size"]) * 512)
    out["(after the last partition)"] = (max(e for _, e in out.values()), os.path.getsize(disk))
    res = {}
    with open(disk, "rb") as f:
        for name, (s, e) in out.items():
            h = hashlib.sha256()
            f.seek(s)
            left = e - s
            while left > 0:
                b = f.read(min(left, 1 << 20))
                if not b:
                    break
                h.update(b)
                left -= len(b)
            res[name] = h.hexdigest()
    return res


def changed(before, after):
    return sorted(n for n in set(before) | set(after) if before.get(n) != after.get(n))


def step(msg):
    print("==> " + msg, flush=True)


def ok(msg):
    print("    OK  " + msg, flush=True)


# ── ramboot ──────────────────────────────────────────────────────────────────
def t_ramboot(a):
    nb = a.netboot
    cmdline = open(os.path.join(nb, "cmdline")).read().strip()
    step("booting the S6000 RAM image directly (kernel + RAM initramfs)")
    vm = VM(qemu(mem=2048, extra=[
        "-no-reboot",
        "-kernel", os.path.join(nb, "vmlinuz"),
        "-initrd", os.path.join(nb, "initrd.img"),
        "-append", "console=ttyS0,115200n8 %s nosaic.selftest panic=5" % cmdline,
    ]), os.path.join(a.out, "ramboot.log"))
    try:
        vm.expect(r"NOSAIC-INITRAMFS starting", 600)
        ok("the initramfs ran")
        vm.expect(r"NOSAIC-BOOT userspace reached", 900)
        ok("init reached userspace")
        # One "FAIL <reason>" line per failed check, then a bare verdict.
        i = vm.expect([r"NOSAIC-SELFTEST OK\r?\n", r"NOSAIC-SELFTEST FAILED\r?\n"], 900)
        text = vm.buf.decode("utf-8", "replace")
        for line in re.findall(r"NOSAIC-SELFTEST (?:note|no data|FAIL \S)[^\r\n]*", text):
            print("        " + line)
        if i != 0:
            raise Fail("self-test failed")
        ok("self-test passed")
    finally:
        vm.stop()


# ── onie: a disk with ONIE embedded, which every other test copies ──────────
def t_onie(a):
    disk = os.path.join(a.out, "onie-disk.raw")
    with open(disk, "wb") as f:
        f.truncate(a.disk_gib << 30)
    step("embedding ONIE onto a blank %d GiB disk from the recovery ISO" % a.disk_gib)
    vm = VM(qemu(disk=disk, cdrom=a.iso), os.path.join(a.out, "onie-embed.log"))
    try:
        vm.expect(r"ONIE: Embed ONIE", 300)
        vm.send("\x1b[B")  # down to "Embed ONIE"
        time.sleep(0.3)
        vm.send("\r")
        vm.expect(r"ONIE: Embedding ONIE", 60)
        ok("chose Embed ONIE")
        # Embedding installs ONIE and reboots; once=d means the reboot comes
        # up from the disk, into ONIE's install mode.
        vm.expect(r"ONIE: Install OS", 1800)
        ok("rebooted from the disk into ONIE's own GRUB")
        vm.expect(r"ONIE:/ #|Please press Enter to activate this console", 900)
        ok("ONIE is running from the disk")
    finally:
        vm.stop()
    sparse_copy(disk, os.path.join(a.out, "onie-golden.raw"))
    names = [p.get("name") for p in partitions(disk)]
    ok("ONIE's layout: %s" % names)


def onie_prompt(vm):
    """From ONIE install mode, stop discovery and get a quiet shell."""
    vm.expect(r"ONIE:/ #|Please press Enter to activate this console", 1200)
    vm.send("\r")
    time.sleep(1)
    vm.line("onie-stop")
    vm.expect(r"ONIE:/ #", 120)
    vm.line("")
    vm.expect(r"ONIE:/ #", 60)


def fresh_disk(a, name):
    src = os.path.join(a.out, "onie-golden.raw")
    if not os.path.exists(src):
        raise Fail("no ONIE disk; run the 'onie' step first")
    dst = os.path.join(a.out, name)
    sparse_copy(src, dst)
    return dst


# ── netboot ──────────────────────────────────────────────────────────────────
def t_control(a):
    """Boot ONIE, stop discovery, and do nothing else: what ONIE writes itself."""
    disk = fresh_disk(a, "control-disk.raw")
    before = regions(disk)
    step("control: boot ONIE and do nothing, to see what ONIE writes on its own")
    vm = VM(qemu(disk=disk), os.path.join(a.out, "control.log"))
    try:
        onie_prompt(vm)
        vm.line("sync")
        vm.expect(r"ONIE:/ #", 60)
        time.sleep(5)
    finally:
        vm.stop()
    diff = changed(before, regions(disk))
    ok("ONIE on its own changed: %s" % (diff or "nothing"))
    return diff


def t_netboot(a):
    # ONIE writes its own state into ONIE-BOOT on every boot, whatever runs
    # after it; the control run measures exactly what. The netboot must change
    # nothing beyond that.
    allowed = set(t_control(a))
    disk = fresh_disk(a, "netboot-disk.raw")
    before = regions(disk)
    bins = [f for f in os.listdir(a.netboot) if f.endswith("-netboot.bin")]
    if not bins:
        raise Fail("no *-netboot.bin in %s" % a.netboot)
    httpd = serve(a.netboot)
    step("from ONIE: onie-nos-install the kexec netboot image")
    vm = VM(qemu(disk=disk), os.path.join(a.out, "netboot.log"))
    try:
        onie_prompt(vm)
        vm.line("which kexec && onie-nos-install http://%s:%d/%s" % (HOST, PORT, bins[0]))
        vm.expect(r"kexec into NOSaic", 600, fail=(r"Kernel panic", r"error: "))
        ok("ONIE downloaded and ran the netboot image")
        vm.expect(r"NOSAIC-INITRAMFS starting", 600)
        ok("kexec landed in NOSaic's initramfs")
        vm.expect(r"NOSAIC-BOOT userspace reached", 900)
        vm.expect(r"login:", 900)
        ok("NOSaic reached a login prompt, from RAM")
    finally:
        vm.stop()
        httpd.shutdown()
        httpd.server_close()
    diff = changed(before, regions(disk))
    extra = [n for n in diff if n not in allowed]
    if extra:
        raise Fail("the netboot changed %s, beyond what ONIE itself writes (%s)"
                   % (extra, sorted(allowed) or "nothing"))
    ok("the disk is unchanged beyond ONIE's own writes (%s changed, as in the control)"
       % (", ".join(diff) or "nothing"))


# ── install ──────────────────────────────────────────────────────────────────
def t_install(a):
    disk = fresh_disk(a, "install-disk.raw")
    onie_before = {p["name"]: p for p in partitions(disk)}
    onie_boot_before = part_bytes(disk, onie_before["ONIE-BOOT"])
    img_dir = os.path.dirname(a.installer)
    httpd = serve(img_dir)
    installer = os.path.basename(a.installer)

    step("from ONIE: onie-nos-install the onie-grub installer")
    vm = VM(qemu(disk=disk), os.path.join(a.out, "install.log"))
    try:
        onie_prompt(vm)
        vm.line("onie-nos-install http://%s:%d/%s" % (HOST, PORT, installer))
        vm.expect(r"NOSaic installed\.", 900, fail=(r"Kernel panic", r"\nerror: "))
        ok("the installer finished")
        vm.expect(r"NOSaic ", 600)  # the GRUB menu entry, after ONIE reboots
        ok("our GRUB menu came up after the reboot")
        vm.expect(r"NOSAIC-BOOT slotdev=(\S+)\s", 900)
        slotdev = re.search(r"NOSAIC-BOOT slotdev=(\S+)", vm.buf.decode("utf-8", "replace")).group(1)
        vm.expect(r"login:", 900)
        ok("NOSaic booted from disk (slot a is %s)" % slotdev)
    finally:
        vm.stop()

    after = {p["name"]: p for p in partitions(disk)}
    for n in ("nosaic-boot", "nosaic-slot-a", "nosaic-slot-b", "nosaic-data"):
        if n not in after:
            raise Fail("partition %s is missing after install: %s" % (n, list(after)))
    ok("all four NOSaic partitions exist by name")
    for n, p in onie_before.items():
        q = after.get(n)
        if not q or (q["start"], q["size"]) != (p["start"], p["size"]):
            raise Fail("ONIE partition %s moved or vanished: %s -> %s" % (n, p, q))
    ok("ONIE's partitions are where they were")
    slot_a_node = sorted(after, key=lambda n: after[n]["start"]).index("nosaic-slot-a") + 1
    if not slotdev.endswith(str(slot_a_node)):
        raise Fail("the initramfs used %s but nosaic-slot-a is partition %d" % (slotdev, slot_a_node))
    ok("the initramfs found slot a by name (%s)" % slotdev)
    if part_bytes(disk, after["ONIE-BOOT"]) != onie_boot_before:
        print("    NOTE ONIE-BOOT's contents changed; ONIE writes its own state there, "
              "so this is checked by booting ONIE below, not by bytes")

    # A/B: write slot b from the running switch, and check it landed on
    # nosaic-slot-b rather than on whatever is third on the disk.
    step("A/B upgrade into slot b from the running system")
    # One server at a time on PORT: the installer's goes before this one.
    httpd.shutdown()
    httpd.server_close()
    httpd2 = serve(os.path.dirname(a.squashfs))
    vm = VM(qemu(disk=disk), os.path.join(a.out, "upgrade.log"))
    try:
        vm.expect(r"login:", 1500)
        login(vm)
        # Services are still starting and printing; let the console settle
        # so the commands below are not interleaved with their output.
        time.sleep(20)
        vm.line("")
        vm.expect(PROMPT, 60)
        # NOSaic configures management from network.conf, which a fresh
        # install does not have. QEMU's user network is 10.0.2.0/24.
        # doas runs with a short PATH, and admin's own PATH has no /sbin, so
        # tools are named by full path, found with w().
        vm.line("w() { for d in /usr/local/bin /usr/bin /bin /usr/sbin /sbin; do "
                "[ -x $d/$1 ] && { echo $d/$1; return; }; done; }; echo W=ok")
        vm.expect(r"W=ok", 60)
        vm.line("I=$(ls /sys/class/net | grep -v '^lo$' | head -1); IP=$(w ip); "
                "doas $IP link set $I up && doas $IP addr add 10.0.2.15/24 dev $I; echo NET=$?")
        vm.expect(r"NET=0", 60, fail=(r"NET=[1-9]",))
        # --disk names the whole disk: the slot is found on it by name, and
        # the boot pointer is the mounted one (upgrade.Disk.State).
        vm.line("D=$(awk '$4 ~ /^[sv]da$/ {print \"/dev/\"$4; exit}' /proc/partitions); "
                "wget -O /tmp/new.sqsh http://%s:%d/%s && "
                "doas $(w nosaic) upgrade install /tmp/new.sqsh --disk $D; echo RC=$?"
                % (HOST, PORT, os.path.basename(a.squashfs)))
        vm.expect(r"RC=0", 900, fail=(r"RC=[1-9]",))
        ok("nosaic upgrade install succeeded")
        vm.line("sync; doas $(w poweroff) -f")
        time.sleep(10)
    finally:
        vm.stop()
        httpd2.shutdown()
        httpd2.server_close()
    parts = {p["name"]: p for p in partitions(disk)}
    if not part_bytes(disk, parts["nosaic-slot-b"]).startswith(b"hsqs"):
        raise Fail("nosaic-slot-b does not hold a squashfs after the upgrade")
    ok("slot b holds the new image")
    for n in ("GRUB-BOOT", "ONIE-BOOT"):
        if n in parts and part_bytes(disk, parts[n])[:4] == b"hsqs":
            raise Fail("%s was overwritten by the upgrade" % n)
    ok("ONIE's partitions were not written by the upgrade")

    step("booting the trial slot, then choosing ONIE from our GRUB menu")
    vm = VM(qemu(disk=disk), os.path.join(a.out, "trial-and-onie.log"))
    try:
        # The pointer must actually move: a trial that silently boots slot a
        # again looks healthy and upgraded nothing.
        i = vm.expect([r"NOSAIC-BOOT-SLOT b\s", r"NOSAIC-BOOT-SLOT a\s"], 900)
        if i != 0:
            raise Fail("the trial boot came up on slot a, not the upgraded slot b")
        vm.expect(r"login:", 900)
        ok("the trial boot came up on slot b")
        login(vm)
        time.sleep(20)
        vm.line("nosaic upgrade status; echo ST=$?")
        vm.expect(r"ST=\d", 120)
        vm.line("doas $(for d in /usr/sbin /sbin /usr/bin /bin; do [ -x $d/reboot ] && echo $d/reboot && break; done)")
        vm.expect(r"NOSaic ", 600)
        vm.send("\x1b[B")  # down from NOSaic to ONIE's entry
        time.sleep(0.3)
        vm.send("\r")
        vm.expect(r"ONIE", 300)
        vm.expect(r"ONIE:/ #|Please press Enter to activate this console", 1200)
        ok("ONIE still boots from our GRUB menu")
    finally:
        vm.stop()


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("tests", nargs="+", choices=["ramboot", "onie", "netboot", "install"])
    ap.add_argument("--netboot", help="the netboot bundle directory")
    ap.add_argument("--installer", help="the onie-grub installer .bin")
    ap.add_argument("--squashfs", help="a rootfs squashfs for the A/B step")
    ap.add_argument("--iso", help="ONIE recovery ISO (kvm_x86_64, legacy BIOS)")
    ap.add_argument("--out", default="rehearsal", help="logs and disks go here")
    ap.add_argument("--disk-gib", type=int, default=6)
    a = ap.parse_args()
    os.makedirs(a.out, exist_ok=True)
    tests = {"ramboot": t_ramboot, "onie": t_onie, "netboot": t_netboot, "install": t_install}
    failed = []
    for t in a.tests:
        try:
            tests[t](a)
            print("PASS " + t, flush=True)
        except Fail as e:
            print("FAIL %s: %s" % (t, e), flush=True)
            failed.append(t)
            if t == "onie":
                break
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
