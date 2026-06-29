package supervisor

import (
	"reflect"
	"sort"

	"github.com/0ploy/zpinit/internal/config"
)

type reloadRestartPair struct {
	// old holds every running replica of the service. For non-replicated
	// services it has exactly one entry; for replicas > 1 it has N. All
	// must stop before the new spec is expanded into a fresh set of
	// runners.
	old []*Runner
	new config.Service
}

type reloadDiff struct {
	add []config.Service
	// remove is a flat list of runners to deregister. Multiple replicas
	// of the same filename produce multiple entries; the order is
	// (filename ASC, replicaIndex ASC) so restart-stop and remove
	// progress predictably.
	remove  []*Runner
	restart []reloadRestartPair
}

// computeDiff is the public-test entry point that takes the lock.
// computeDiffLocked is the internal version used by Reload while it
// already holds o.mu.RLock.
func (o *Orchestrator) computeDiff(newCfg *config.Config) reloadDiff {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.computeDiffLocked(newCfg)
}

// computeDiffLocked produces a stable, filename-sorted action list.
// Pure function aside from reading o.runners; caller must hold o.mu.
//
// Replicated services map a single filename to N runners, all sharing
// the same Spec(). The diff key remains the filename: a change to the
// service spec restarts every replica; an unchanged spec leaves all
// replicas alone.
//
// Three independent triggers can land a service in diff.restart:
//
//  1. Spec change. servicesEqual reports the loaded TOML differs from
//     the running runners' Spec (ignoring Filename and the dynamic N
//     for auto services). Per-filename diff walk below.
//  2. globals.Env change. The orchestrator's cfg-level [env] map moved
//     and every reloadable service that survives the per-filename
//     walk needs a restart so its next spawn picks up the new merged
//     env. Children can't be re-env'd in place. Tail block of this
//     function.
//  3. Resource snapshot change (cpu/memory). OnResourceChange owns
//     this trigger; the per-runner reload (signal/command/restart) is
//     dispatched outside computeDiffLocked via reloadAcrossGroups and
//     does not show up in this diff. Listed here so the full picture
//     of "what makes a service restart" lives in one place.
//
// All three honor reloadable=false: a non-reloadable service is left
// running with its existing config and env regardless of trigger.
func (o *Orchestrator) computeDiffLocked(newCfg *config.Config) reloadDiff {
	existing := map[string][]*Runner{}
	for _, r := range o.runners {
		fn := r.Cfg().Filename
		existing[fn] = append(existing[fn], r)
	}
	newSet := map[string]config.Service{}
	for _, s := range newCfg.Services {
		newSet[s.Filename] = s
	}
	// Files the loader could not parse/validate are absent from
	// newSet. Without this guard they'd look "removed" and we'd stop a
	// healthy running service because someone fat-fingered its file. A
	// parse error is "no opinion": leave whatever is running for that
	// filename untouched until the file is fixed.
	skipped := map[string]config.FileError{}
	for _, fe := range newCfg.SkippedFiles {
		skipped[fe.File] = fe
	}

	allFiles := map[string]struct{}{}
	for fn := range existing {
		allFiles[fn] = struct{}{}
	}
	for fn := range newSet {
		allFiles[fn] = struct{}{}
	}
	ordered := make([]string, 0, len(allFiles))
	for fn := range allFiles {
		ordered = append(ordered, fn)
	}
	sort.Strings(ordered)

	var diff reloadDiff
	for _, fn := range ordered {
		oldRunners, hasOld := existing[fn]
		s, hasNew := newSet[fn]
		switch {
		case hasOld && !hasNew:
			if fe, isSkipped := skipped[fn]; isSkipped {
				o.log.Warn("reload: service file failed to parse/validate; keeping running service unchanged",
					"file", fn, "err", fe.Err)
				continue
			}
			diff.remove = append(diff.remove, oldRunners...)
		case !hasOld && hasNew:
			diff.add = append(diff.add, s)
		case hasOld && hasNew:
			// All replicas share the same Spec (filename is the key).
			oldSpec := oldRunners[0].Spec()
			if !servicesEqual(oldSpec, s) {
				if oldSpec.IsReloadable() {
					diff.restart = append(diff.restart, reloadRestartPair{old: oldRunners, new: s})
				} else {
					o.log.Info("reload: config changed but reloadable=false; ignoring",
						"service", oldSpec.Name, "file", fn)
				}
			}
		}
	}

	// globals.Env change propagation: every reloadable service that
	// isn't already going to be torn down needs a restart so its next
	// spawn picks up the new merged env. Long-running children can't
	// be retroactively given new env vars, so restart is the only
	// mechanism. Services with reloadable=false stay running with
	// stale env, matching the contract for the rest of the spec.
	if !envMapsEqual(o.cfg.Globals.Env, newCfg.Globals.Env) {
		inDiff := map[string]struct{}{}
		for _, r := range diff.remove {
			inDiff[r.Cfg().Filename] = struct{}{}
		}
		for _, p := range diff.restart {
			inDiff[p.old[0].Cfg().Filename] = struct{}{}
		}
		for fn, runners := range existing {
			if _, skip := inDiff[fn]; skip {
				continue
			}
			newSpec, ok := newSet[fn]
			if !ok {
				continue // already in remove
			}
			spec := runners[0].Spec()
			if !spec.IsReloadable() {
				o.log.Info("reload: globals.env changed but reloadable=false; service keeps old env",
					"service", spec.Name)
				continue
			}
			diff.restart = append(diff.restart, reloadRestartPair{old: runners, new: newSpec})
		}
	}
	return diff
}

// envMapsEqual reports whether two env maps are key/value identical. Nil
// and empty maps compare equal (consistent with reflect.DeepEqual on
// these types).
func envMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// sortRunners orders runners by (Filename, replicaIndex). Filename is
// the primary boot-order key; replicaIndex is the tiebreaker for
// services declared with replicas > 1 so `zpctl status` shows
// consumer/0 above consumer/1.
func sortRunners(rs []*Runner) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.Cfg().Filename != b.Cfg().Filename {
			return a.Cfg().Filename < b.Cfg().Filename
		}
		return a.replicaIndex < b.replicaIndex
	})
}

// servicesEqual compares two service configs for reload-diff purposes.
// Ignores Filename (the diff key, not a content field) and the dynamic
// replica count for auto services (the scaler owns it; the file reload
// from disk has N=0 by definition).
//
// Normalizes nil-vs-empty for every map/slice field and resolves the
// Reloadable *bool pointer to its effective value before comparison.
// Without the normalization, reflect.DeepEqual reports phantom
// inequality for cosmetic edits that don't change semantics:
//
//   - `env = {}` vs no `env` key
//   - `reloadable = true` vs no key (both mean reloadable)
//   - `reload_on_change = []` vs no key (both disable the trigger)
//   - `command = []` vs no key (both invalid; caught by validate)
//
// Each is niche on its own, but together they tie reload behavior to
// TOML stylistic choices rather than to what the operator means.
func servicesEqual(a, b config.Service) bool {
	a.Filename, b.Filename = "", ""
	if a.Replicas.Auto {
		a.Replicas.N = 0
	}
	if b.Replicas.Auto {
		b.Replicas.N = 0
	}
	// Effective reloadable value (nil == true; matches IsReloadable).
	aRel := a.Reloadable == nil || *a.Reloadable
	bRel := b.Reloadable == nil || *b.Reloadable
	if aRel != bRel {
		return false
	}
	a.Reloadable, b.Reloadable = nil, nil
	// Nil-vs-empty normalization on every field DeepEqual would
	// distinguish. Cheap: typical service configs touch a handful of
	// these, the rest are no-ops.
	normalizeMap(&a.Env, &b.Env)
	normalizeSlice(&a.Command, &b.Command)
	normalizeSlice(&a.ReloadCommand, &b.ReloadCommand)
	normalizeSlice(&a.ReloadOnChange, &b.ReloadOnChange)
	return reflect.DeepEqual(a, b)
}

func normalizeMap(a, b *map[string]string) {
	if *a == nil && *b == nil {
		return
	}
	if *a == nil {
		*a = map[string]string{}
	}
	if *b == nil {
		*b = map[string]string{}
	}
}

func normalizeSlice(a, b *[]string) {
	if *a == nil && *b == nil {
		return
	}
	if *a == nil {
		*a = []string{}
	}
	if *b == nil {
		*b = []string{}
	}
}
