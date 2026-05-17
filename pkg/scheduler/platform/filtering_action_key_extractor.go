package platform

import (
	"context"
	"sort"

	remoteexecution "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	"github.com/buildbarn/bb-storage/pkg/digest"
)

type filteringActionKeyExtractor struct {
	ignoredNames map[string]struct{}
}

// NewFilteringActionKeyExtractor returns a KeyExtractor that reads
// platform properties from the REv2 Action message but drops the
// designated property names before computing the routing key.
//
// This is intended for "resource hint" properties that the worker
// honors at execution time (e.g. "cpu", "memory", "gpu" used for
// cgroup placement) but that should not partition the scheduler's
// per-platform queues.
func NewFilteringActionKeyExtractor(ignoredNames []string) KeyExtractor {
	m := make(map[string]struct{}, len(ignoredNames))
	for _, n := range ignoredNames {
		m[n] = struct{}{}
	}
	return &filteringActionKeyExtractor{ignoredNames: m}
}

func (e *filteringActionKeyExtractor) ExtractKey(ctx context.Context, digestFunction digest.Function, action *remoteexecution.Action) (Key, error) {
	platform := action.Platform
	if platform == nil || len(e.ignoredNames) == 0 {
		return NewKey(digestFunction.GetInstanceName(), platform)
	}
	filtered := make([]*remoteexecution.Platform_Property, 0, len(platform.Properties))
	for _, p := range platform.Properties {
		if _, drop := e.ignoredNames[p.Name]; drop {
			continue
		}
		filtered = append(filtered, p)
	}
	// NewKey requires properties sorted by (name, value). The input
	// was already validated as sorted on the client side, but a
	// defensive sort here keeps us correct if the upstream contract
	// ever loosens.
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].Name != filtered[j].Name {
			return filtered[i].Name < filtered[j].Name
		}
		return filtered[i].Value < filtered[j].Value
	})
	return NewKey(digestFunction.GetInstanceName(), &remoteexecution.Platform{Properties: filtered})
}
