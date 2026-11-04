package service

import (
	"os/user"
	"strconv"
	"testing"
)

func TestResolveIdentityRootDefault(t *testing.T) {
	id, err := ResolveIdentity("")
	if err != nil {
		t.Fatal(err)
	}
	if id.Cred.Uid != 0 || id.Cred.Gid != 0 {
		t.Errorf("empty spec = %+v, want root", id.Cred)
	}
	if id.Cred.Groups == nil {
		t.Error("Groups must be set (empty), so root's supplementary groups are dropped")
	}
}

func TestResolveIdentityNumericWithoutPasswd(t *testing.T) {
	// A uid with no passwd entry runs as itself with gid 0, as under
	// container runtimes.
	id, err := ResolveIdentity("4242424")
	if err != nil {
		t.Fatal(err)
	}
	if id.Cred.Uid != 4242424 || id.Cred.Gid != 0 || id.Home != "" {
		t.Errorf("got %+v home=%q", id.Cred, id.Home)
	}
	id, err = ResolveIdentity("4242424:777")
	if err != nil {
		t.Fatal(err)
	}
	if id.Cred.Gid != 777 {
		t.Errorf("explicit gid: %+v", id.Cred)
	}
}

func TestResolveIdentityByName(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Skip(err)
	}
	id, err := ResolveIdentity(u.Username)
	if err != nil {
		t.Fatal(err)
	}
	if strconv.Itoa(int(id.Cred.Uid)) != u.Uid || strconv.Itoa(int(id.Cred.Gid)) != u.Gid || id.Home != u.HomeDir {
		t.Errorf("got %+v home=%q, want uid=%s gid=%s home=%s", id.Cred, id.Home, u.Uid, u.Gid, u.HomeDir)
	}
	if _, err := ResolveIdentity("no-such-user-zpinit"); err == nil {
		t.Error("unknown user name accepted")
	}
	if _, err := ResolveIdentity(u.Username + ":no-such-group-zpinit"); err == nil {
		t.Error("unknown group name accepted")
	}
	if _, err := ResolveIdentity(u.Username + ":"); err == nil {
		t.Error("empty group accepted")
	}
}
