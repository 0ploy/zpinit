package machine

import (
	"testing"
	"time"
)

func TestOOMKilledIn(t *testing.T) {
	recs := [][]byte{
		[]byte("6,100,5000000,-;eth0: link up\n"),
		[]byte("3,101,9000000,-;Out of memory: Killed process 4321 (php-fpm) total-vm:1024kB, anon-rss:900kB\n SUBSYSTEM=mm\n"),
		[]byte("3,102,9500000,-;Memory cgroup out of memory: Killed process 77 (node) total-vm:1kB\n"),
		[]byte("6,103,9600000,-;oom_reaper: reaped process 4321 (php-fpm), now anon-rss:0kB\n"),
		[]byte("garbage without a separator"),
	}
	cases := []struct {
		name  string
		pid   int
		since time.Duration
		want  bool
	}{
		{"global oom", 4321, 8 * time.Second, true},
		{"memcg oom", 77, 9 * time.Second, true},
		{"pid prefix is not a match", 432, 0, false},
		{"pid suffix is not a match", 21, 0, false},
		{"other pid", 999, 0, false},
		// Spawned after the kill (beyond the slack): an earlier process
		// that had the same pid.
		{"reused pid", 4321, 20 * time.Second, false},
		// Within the slack the record still counts.
		{"clock slack", 4321, 10 * time.Second, true},
	}
	for _, c := range cases {
		if got := oomKilledIn(recs, c.pid, c.since); got != c.want {
			t.Errorf("%s: oomKilledIn(pid=%d, since=%v) = %v, want %v", c.name, c.pid, c.since, got, c.want)
		}
	}
}

func TestParseKmsgRecord(t *testing.T) {
	ts, msg, ok := parseKmsgRecord([]byte("4,7,1500,-,caller=T1;hello world\n KEY=v\n"))
	if !ok || ts != 1500*time.Microsecond || string(msg) != "hello world" {
		t.Errorf("got ts=%v msg=%q ok=%v", ts, msg, ok)
	}
	for _, bad := range []string{"", "no separator", "1,2;msg", "1,2,x,-;msg"} {
		if _, _, ok := parseKmsgRecord([]byte(bad)); ok {
			t.Errorf("parsed %q", bad)
		}
	}
}
