package service

import (
	"reflect"
	"testing"
)

// Map iteration is randomised, so appending override-only keys straight
// from the range loop made a service's env slice differ run to run.
// That makes `zpinit --plan` output non-reproducible.
func TestMergeEnv_AppendOrderIsDeterministic(t *testing.T) {
	base := []string{"PATH=/usr/bin", "HOME=/root"}
	override := map[string]string{
		"ZED": "1", "ALPHA": "2", "MIKE": "3", "BRAVO": "4", "YANKEE": "5",
	}
	want := MergeEnv(base, override)
	for i := 0; i < 50; i++ {
		if got := MergeEnv(base, override); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d differs:\n got %v\nwant %v", i, got, want)
		}
	}
	// Base order is preserved; the appended tail is sorted.
	expect := []string{"PATH=/usr/bin", "HOME=/root",
		"ALPHA=2", "BRAVO=4", "MIKE=3", "YANKEE=5", "ZED=1"}
	if !reflect.DeepEqual(want, expect) {
		t.Errorf("got %v, want %v", want, expect)
	}
}

// Keys already in base are replaced IN PLACE, keeping base's position,
// and must not also be appended.
func TestMergeEnv_InPlaceReplacementKeepsPosition(t *testing.T) {
	got := MergeEnv([]string{"A=1", "B=2", "C=3"}, map[string]string{"B": "new", "D": "4"})
	want := []string{"A=1", "B=new", "C=3", "D=4"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
