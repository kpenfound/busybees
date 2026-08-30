package main

import (
	"strings"
	"testing"
)

// The --fix flag is what wires doctor.ApplyFixes into the command; the repair
// itself is tested in internal/doctor.
func TestDoctorHasAFixFlag(t *testing.T) {
	cmd := newDoctorCmd(&globalFlags{})
	f := cmd.Flags().Lookup("fix")
	if f == nil {
		t.Fatal("bees doctor has no --fix flag")
	}
	if f.Value.Type() != "bool" || f.DefValue != "false" {
		t.Errorf("--fix is %s = %s, want a bool defaulting to false: doctor must change nothing unless asked", f.Value.Type(), f.DefValue)
	}
	for _, want := range []string{"--fix", "base label", "bees.toml"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("`bees doctor --help` does not mention %q", want)
		}
	}
}
