package config

import "fmt"

// Replicas is the per-service "replicas" value. Decodes from TOML
// as either an integer (`replicas = 3`) or the string `"auto"`,
// the latter meaning "let zpinit compute the count from the
// detected CPU budget at runtime."
//
// N is the effective replica count at any moment:
//
//   - Static services (Auto == false): the operator-set integer
//     (defaults to 1 in applyServiceDefaults if the key is absent).
//   - Auto services (Auto == true): set by applyServiceDefaults at
//     boot to the initial auto target, then updated by the
//     orchestrator's scaler on each watcher-driven commit.
type Replicas struct {
	N    int
	Auto bool
}

// UnmarshalTOML implements toml.Unmarshaler so the same `replicas`
// key accepts both the integer and string forms.
func (r *Replicas) UnmarshalTOML(v interface{}) error {
	switch x := v.(type) {
	case int64:
		// 0 is rejected here rather than in validation because
		// applyServiceDefaults turns an unset (zero) N into 1: an
		// explicit `replicas = 0` would silently become one running
		// copy, the opposite of what the operator asked for.
		if x < 1 {
			return fmt.Errorf("replicas: must be >= 1 (got %d); to park a service, rename the file to *.disabled", x)
		}
		r.N = int(x)
		return nil
	case string:
		if x != "auto" {
			return fmt.Errorf(`replicas: unknown string %q (only "auto" allowed)`, x)
		}
		r.Auto = true
		return nil
	}
	return fmt.Errorf("replicas: must be integer or \"auto\", got %T", v)
}

// MarshalText lets the type round-trip through toml encoding for
// human-readable output (zpctl status / --check-config diff). Not
// load-bearing for production decode; we keep it tiny.
func (r Replicas) MarshalText() ([]byte, error) {
	if r.Auto {
		return []byte("auto"), nil
	}
	return []byte(fmt.Sprintf("%d", r.N)), nil
}
