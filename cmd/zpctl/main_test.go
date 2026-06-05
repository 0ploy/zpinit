package main

import "testing"

func TestIsWaitCmd(t *testing.T) {
	cases := []struct {
		args []string
		want bool
	}{
		{[]string{"start", "--wait", "api"}, true},
		{[]string{"start", "api", "--wait"}, true},
		{[]string{"restart", "--wait", "all"}, true},
		{[]string{"start", "api"}, false},
		{[]string{"stop", "--wait", "api"}, false}, // stop never waits
		{[]string{"status", "--wait"}, false},
	}
	for _, c := range cases {
		if got := isWaitCmd(c.args); got != c.want {
			t.Errorf("isWaitCmd(%v) = %v, want %v", c.args, got, c.want)
		}
	}
}

func TestIsStreamingCmd(t *testing.T) {
	if !isStreamingCmd([]string{"tail", "--follow", "api"}) {
		t.Error("tail --follow should stream")
	}
	if !isStreamingCmd([]string{"tail", "-f", "api"}) {
		t.Error("tail -f should stream")
	}
	if isStreamingCmd([]string{"tail", "api"}) {
		t.Error("plain tail should not stream")
	}
	if isStreamingCmd([]string{"start", "--wait", "api"}) {
		t.Error("start --wait is long-running but not streaming")
	}
}
