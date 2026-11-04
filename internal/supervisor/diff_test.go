package supervisor

import (
	"reflect"
	"testing"

	"github.com/0ploy/zpinit/internal/config"
)

// Writing out a pointer field's default is not a change: a reread over
// it must not restart the service.
func TestServicesEqualPointerDefaults(t *testing.T) {
	yes, no := true, false
	five, three := config.DefaultMaxConsecutiveCrashes, 3
	base := config.Service{Name: "web", Command: []string{"web"}}
	with := func(f func(*config.Service)) config.Service {
		s := base
		f(&s)
		return s
	}
	same := map[string]config.Service{
		"reloadable = true":           with(func(s *config.Service) { s.Reloadable = &yes }),
		"pdeathsig = true":            with(func(s *config.Service) { s.Pdeathsig = &yes }),
		"max_consecutive_crashes = 5": with(func(s *config.Service) { s.MaxConsecutiveCrashes = &five }),
	}
	for name, s := range same {
		if !servicesEqual(base, s) || !servicesEqual(s, base) {
			t.Errorf("%s vs unset: unequal, want equal", name)
		}
	}
	differ := map[string]config.Service{
		"reloadable = false":          with(func(s *config.Service) { s.Reloadable = &no }),
		"pdeathsig = false":           with(func(s *config.Service) { s.Pdeathsig = &no }),
		"max_consecutive_crashes = 3": with(func(s *config.Service) { s.MaxConsecutiveCrashes = &three }),
	}
	for name, s := range differ {
		if servicesEqual(base, s) || servicesEqual(s, base) {
			t.Errorf("%s vs unset: equal, want unequal", name)
		}
	}
}

// servicesEqual resolves every pointer field to its effective value.
// A new one added to config.Service must be handled there and listed
// here; otherwise its default value reads as a change.
func TestServicesEqualKnowsEveryPointerField(t *testing.T) {
	handled := map[string]bool{
		"Reloadable": true, "Pdeathsig": true, "MaxConsecutiveCrashes": true,
		// nil means no probe, not a default; compared by value.
		"Ready": true,
	}
	typ := reflect.TypeOf(config.Service{})
	for i := range typ.NumField() {
		f := typ.Field(i)
		if f.Type.Kind() == reflect.Pointer && !handled[f.Name] {
			t.Errorf("config.Service.%s is a pointer servicesEqual does not normalize", f.Name)
		}
	}
}
