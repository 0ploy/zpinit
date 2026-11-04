package service

import (
	"fmt"
	"strconv"
	"strings"
	"syscall"
)

// Identity is a resolved `user[:group]` spec: the credential a child
// runs with, plus what its env needs.
type Identity struct {
	Cred *syscall.Credential
	// Name and Home are the user's passwd name and home directory, or ""
	// when the user is a bare numeric id with no entry.
	Name string
	Home string
}

// ResolveIdentity resolves a Docker-style `user[:group]` spec (names or
// numeric ids) the way container runtimes do: a name must exist; a
// numeric uid may have no passwd entry, and then runs with gid 0 and no
// supplementary groups; otherwise the primary group comes from passwd
// (or the explicit group) and the supplementary groups from the group
// database. An empty spec means root.
//
// The returned credential always sets the group list, so a child of a
// root PID 1 never inherits root's supplementary groups.
func ResolveIdentity(spec string) (*Identity, error) {
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	if userPart == "" {
		userPart = "0"
	}
	var (
		uid, gid uint32
		name     string
		home     string
		groups   []uint32
	)
	u, err := lookupUser(userPart)
	switch {
	case err == nil:
		uid, err = parseID(u.Uid)
		if err != nil {
			return nil, fmt.Errorf("user %q: %w", userPart, err)
		}
		gid, err = parseID(u.Gid)
		if err != nil {
			return nil, fmt.Errorf("user %q: %w", userPart, err)
		}
		name, home = u.Username, u.HomeDir
		if ids, gerr := u.GroupIds(); gerr == nil {
			for _, s := range ids {
				if g, perr := parseID(s); perr == nil && g != gid {
					groups = append(groups, g)
				}
			}
		}
	case isNumeric(userPart):
		uid, err = parseID(userPart)
		if err != nil {
			return nil, fmt.Errorf("user %q: %w", userPart, err)
		}
	default:
		return nil, fmt.Errorf("user %q: %w", userPart, err)
	}
	if hasGroup {
		if groupPart == "" {
			return nil, fmt.Errorf("empty group in %q", spec)
		}
		g, err := lookupGroup(groupPart)
		switch {
		case err == nil:
			gid, err = parseID(g.Gid)
			if err != nil {
				return nil, fmt.Errorf("group %q: %w", groupPart, err)
			}
		case isNumeric(groupPart):
			gid, err = parseID(groupPart)
			if err != nil {
				return nil, fmt.Errorf("group %q: %w", groupPart, err)
			}
		default:
			return nil, fmt.Errorf("group %q: %w", groupPart, err)
		}
	}
	if groups == nil {
		groups = []uint32{}
	}
	return &Identity{
		Cred: &syscall.Credential{Uid: uid, Gid: gid, Groups: groups},
		Name: name,
		Home: home,
	}, nil
}

func parseID(s string) (uint32, error) {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse id %q: %w", s, err)
	}
	return uint32(v), nil
}
