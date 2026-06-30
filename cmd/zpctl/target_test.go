package main

import (
	"slices"
	"testing"
)

func TestTranslateSupervisorTarget(t *testing.T) {
	cases := []struct {
		in, want string
		wantErr  bool
	}{
		// No colon: pass through untouched (native forms, flags, "all").
		{in: "consumer", want: "consumer"},
		{in: "consumer/2", want: "consumer/2"},
		{in: "all", want: "all"},
		{in: "--wait", want: "--wait"},
		{in: "HUP", want: "HUP"},
		// group:* -> all replicas of the group.
		{in: "consumer:*", want: "consumer"},
		// group:group (numprocs=1 default) -> all replicas.
		{in: "consumer:consumer", want: "consumer"},
		// group:group_N (default numprocs>1 naming) -> replica N.
		{in: "consumer:consumer_2", want: "consumer/2"},
		{in: "consumer:consumer_02", want: "consumer/2"},
		// Group names may contain underscores; anchoring on the group
		// from the left of the colon keeps the index unambiguous.
		{in: "my_worker:my_worker_3", want: "my_worker/3"},
		// Unrecognized process suffix: reject, don't widen to the group.
		{in: "consumer:other", wantErr: true},
		{in: "consumer:consumer_x", wantErr: true},
		{in: "consumer:", wantErr: true},
	}
	for _, c := range cases {
		got, err := translateSupervisorTarget(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestTranslateTargets(t *testing.T) {
	// A full verb arg list: flags and signal names survive, targets are
	// rewritten in place.
	got, err := translateTargets([]string{"--wait", "worker:*", "api:api_1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"--wait", "worker", "api/1"}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}

	if _, err := translateTargets([]string{"worker:bogus"}); err == nil {
		t.Error("expected error for unrecognized target, got nil")
	}
}
