/*
Copyright 2025 Flant JSC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package gc is a generic, cron-triggered garbage collector for terminal Kubernetes objects. It is
// ported from the Deckhouse virtualization module
// (images/virtualization-artifact/pkg/controller/gc, Apache-2.0, Flant JSC) with two deliberate
// changes for storage-foundation: retention age is measured from a caller-supplied time extractor
// (status.completionTimestamp) instead of creationTimestamp, and the index/cap logic is off by default
// (callers pass maxCount=0). The virtualization-local dependencies (pkg/common/object, the module's
// logger wrapper) are replaced with local analogs and controller-runtime's logr.
package gc

import (
	"cmp"
	"slices"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

type (
	// IsCandidate reports whether an object is eligible for garbage collection (e.g. it is in a terminal
	// phase). Non-candidates are always retained.
	IsCandidate func(obj client.Object) bool
	// IndexFunc groups objects for the optional per-index cap (only consulted when maxCount > 0).
	IndexFunc func(obj client.Object) string
	// AgeTimeFunc extracts the reference time from which an object's retention age is measured. For the
	// execution objects this is status.completionTimestamp (when the object became terminal), NOT
	// creationTimestamp — a transfer may run for hours before completing, and creation-based age would
	// reap a freshly-completed object. A zero time means "no reference yet": the object is treated as
	// age 0 and is never expired (fail-safe against reaping a terminal object that lacks the timestamp).
	AgeTimeFunc func(obj client.Object) time.Time
)

// DefaultFilter returns the subset of objs that should be garbage-collected: candidates that are not
// terminating and whose age (now - ageTime(obj)) exceeds ttl. When maxCount > 0 it additionally caps the
// retained (non-expired) objects per index, appending the overflow to the result; storage-foundation
// passes maxCount = 0, disabling the cap entirely.
func DefaultFilter(objs []client.Object, isCandidate IsCandidate, ttl time.Duration, ageTime AgeTimeFunc, indexFunc IndexFunc, maxCount int, now time.Time) []client.Object {
	var (
		expired    []client.Object
		nonExpired []client.Object
	)

	for _, obj := range objs {
		if !isCandidate(obj) {
			continue
		}

		if isTerminating(obj) {
			continue
		}

		if getAge(obj, ageTime, now) > ttl {
			expired = append(expired, obj)
			continue
		}

		nonExpired = append(nonExpired, obj)
	}

	result := expired
	if maxCount <= 0 || indexFunc == nil {
		return result
	}

	slices.SortFunc(nonExpired, func(a, b client.Object) int {
		return cmp.Compare(getAge(a, ageTime, now), getAge(b, ageTime, now))
	})

	// Keep the maxCount youngest objects for each index; the rest overflow into the delete set.
	indexed := make(map[string]int)
	for _, obj := range nonExpired {
		index := indexFunc(obj)
		count := indexed[index]
		if count >= maxCount {
			result = append(result, obj)
		}
		indexed[index]++
	}

	return result
}

func getAge(obj client.Object, ageTime AgeTimeFunc, now time.Time) time.Duration {
	t := ageTime(obj)
	if t.IsZero() {
		return 0
	}
	return now.Sub(t).Truncate(time.Second)
}

// isTerminating reports whether the object is already being deleted (a local analog of the
// virtualization pkg/common/object.IsTerminating helper).
func isTerminating(obj client.Object) bool {
	return !obj.GetDeletionTimestamp().IsZero()
}
