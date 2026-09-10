package supervisor

import (
	"reflect"
	"testing"
)

// extractFlag used to filter into args[:0], overwriting the caller's
// backing array. cmdTailFollow calls it twice in a row on the same
// slice, and anything reading req.Args after dispatch would have seen
// truncated data.
func TestExtractFlag_DoesNotMutateCaller(t *testing.T) {
	args := []string{"--verbose", "redis", "--json", "nginx"}
	original := append([]string(nil), args...)

	verbose, rest := extractFlag(args, "--verbose")
	if !verbose {
		t.Error("--verbose not detected")
	}
	if !reflect.DeepEqual(args, original) {
		t.Errorf("caller's slice was mutated: %v, want %v", args, original)
	}
	if want := []string{"redis", "--json", "nginx"}; !reflect.DeepEqual(rest, want) {
		t.Errorf("rest = %v, want %v", rest, want)
	}

	// Chained extraction, the cmdStatus / cmdTailFollow pattern.
	jsonOut, rest2 := extractFlag(rest, "--json")
	if !jsonOut {
		t.Error("--json not detected")
	}
	if want := []string{"redis", "nginx"}; !reflect.DeepEqual(rest2, want) {
		t.Errorf("rest2 = %v, want %v", rest2, want)
	}
	if !reflect.DeepEqual(args, original) {
		t.Errorf("caller's slice mutated by the chained call: %v, want %v", args, original)
	}
}

func TestExtractFlag_MultipleOccurrencesAndAbsence(t *testing.T) {
	// Repeated flags are all stripped, so an operator writing it twice
	// doesn't get "unknown service: --verbose".
	found, rest := extractFlag([]string{"--verbose", "a", "--verbose"}, "--verbose")
	if !found || !reflect.DeepEqual(rest, []string{"a"}) {
		t.Errorf("found=%v rest=%v", found, rest)
	}
	found, rest = extractFlag([]string{"a", "b"}, "--verbose")
	if found || !reflect.DeepEqual(rest, []string{"a", "b"}) {
		t.Errorf("found=%v rest=%v", found, rest)
	}
	// Nil in, empty out, no panic.
	if found, rest = extractFlag(nil, "--verbose"); found || len(rest) != 0 {
		t.Errorf("found=%v rest=%v", found, rest)
	}
}
