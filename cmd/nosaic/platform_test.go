package main

import "testing"

// Off a switch the S6000's driver cannot open: its iSMT functions are not on
// this machine's PCI bus. That is exactly the failure the boot service must
// survive and an operator must be told about.
func TestReleaseASICAtBootSurvivesAPlatformThatCannotOpen(t *testing.T) {
	if err := platformCmd([]string{"--board", "dell-s6000-on", "release-asic", "--boot"}); err != nil {
		t.Fatalf("the boot service's form failed, which takes s6-rc's change down with it: %v", err)
	}
	if err := platformCmd([]string{"--board", "dell-s6000-on", "release-asic"}); err == nil {
		t.Fatal("typed by hand, a platform that cannot open should be an error")
	}
}
