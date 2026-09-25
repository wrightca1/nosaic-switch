package upgrade

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gptDisk makes a sparse disk image partitioned by an sfdisk script.
func gptDisk(t *testing.T, script string) string {
	t.Helper()
	if _, err := exec.LookPath("sfdisk"); err != nil {
		t.Skip("sfdisk not available")
	}
	path := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, 64<<20); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sfdisk", "--quiet", path)
	cmd.Stdin = strings.NewReader(script)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sfdisk: %v\n%s", err, b)
	}
	return path
}

// The shape an ONIE x86 installer leaves: ONIE's own partitions first, ours
// after. Positional lookup would make ONIE-BOOT slot a.
func TestOurPartitionsAreFoundByNameBehindONIE(t *testing.T) {
	path := gptDisk(t, `label: gpt
size=1MiB, type=21686148-6449-6E6F-744E-656564454649, name="GRUB-BOOT"
size=8MiB, type=linux, name="ONIE-BOOT"
size=4MiB, type=linux, name="nosaic-boot"
size=8MiB, type=linux, name="nosaic-slot-a"
size=8MiB, type=linux, name="nosaic-slot-b"
size=8MiB, type=linux, name="nosaic-data"
`)
	parts, err := Disk{Path: path}.ourPartitions()
	if err != nil {
		t.Fatal(err)
	}
	all, err := Disk{Path: path}.partitions()
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range partNames {
		if parts[i].Name != want {
			t.Errorf("index %d is %q, want %q", i, parts[i].Name, want)
		}
		if parts[i].Start != all[i+2].Start {
			t.Errorf("%s starts at %d, want %d", want, parts[i].Start, all[i+2].Start)
		}
	}
}

func TestOurPartitionsRefusesAHalfNamedLayout(t *testing.T) {
	path := gptDisk(t, `label: gpt
size=8MiB, type=linux, name="ONIE-BOOT"
size=4MiB, type=linux, name="nosaic-boot"
size=8MiB, type=linux, name="nosaic-slot-a"
size=8MiB, type=linux, name="nosaic-data"
`)
	if _, err := (Disk{Path: path}).ourPartitions(); err == nil ||
		!strings.Contains(err.Error(), "nosaic-slot-b") {
		t.Fatalf("want a refusal naming nosaic-slot-b, got %v", err)
	}
}

// A DOS table has no names, and is only ever NOSaic's own whole-disk layout.
func TestOurPartitionsIsPositionalWithoutNames(t *testing.T) {
	path := gptDisk(t, `label: dos
size=4MiB, type=83
size=8MiB, type=83
size=8MiB, type=83
type=83
`)
	parts, err := Disk{Path: path}.ourPartitions()
	if err != nil {
		t.Fatal(err)
	}
	all, _ := Disk{Path: path}.partitions()
	if len(parts) != len(all) || parts[1].Start != all[1].Start {
		t.Fatalf("positional layout changed: %+v vs %+v", parts, all)
	}
}

// A running S6000: slots are partitions behind ONIE's, and the boot pointer
// is the mounted one. The image must land on nosaic-slot-b by name, the
// pointer must change in the state directory, and ONIE must not be touched.
func TestLiveInstallWritesTheNamedSlotAndTheMountedPointer(t *testing.T) {
	path := gptDisk(t, `label: gpt
size=1MiB, type=21686148-6449-6E6F-744E-656564454649, name="GRUB-BOOT"
size=8MiB, type=linux, name="ONIE-BOOT"
size=4MiB, type=linux, name="nosaic-boot"
size=8MiB, type=linux, name="nosaic-slot-a"
size=8MiB, type=linux, name="nosaic-slot-b"
size=8MiB, type=linux, name="nosaic-data"
`)
	state := t.TempDir()
	if err := writeStateDir(state, map[string]string{"active": "a"}); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	img := squashfsImage(t, t.TempDir(), "new.sqsh", 4096)
	before, _ := os.ReadFile(path)

	d := Disk{Path: path, State: state, Data: data}
	if err := Install(d, "b", img); err != nil {
		t.Fatalf("install: %v", err)
	}

	after, _ := os.ReadFile(path)
	all, _ := Disk{Path: path}.partitions()
	byName := map[string]partition{}
	for _, p := range all {
		byName[p.Name] = p
	}
	b := byName["nosaic-slot-b"]
	if string(after[b.Start*512:b.Start*512+4]) != "hsqs" {
		t.Error("the image is not at the start of nosaic-slot-b")
	}
	for _, n := range []string{"GRUB-BOOT", "ONIE-BOOT", "nosaic-boot", "nosaic-slot-a", "nosaic-data"} {
		p := byName[n]
		lo, hi := p.Start*512, (p.Start+p.Size)*512
		if string(after[lo:hi]) != string(before[lo:hi]) {
			t.Errorf("%s changed; only nosaic-slot-b should have been written", n)
		}
	}
	st, err := readStateDir(state)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(st["trial"]) != "b" {
		t.Errorf("the mounted pointer has trial %q, want b", st["trial"])
	}
}
