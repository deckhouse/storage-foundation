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

package gc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The GC filter is generic over client.Object; a ConfigMap is a convenient concrete object whose
// annotations carry the per-object test fixture (terminal flag + completion time).
const (
	annTerminal   = "test/terminal"
	annCompletion = "test/completionTime"
	annIndex      = "test/index"
)

func obj(name string, terminal bool, completion time.Time) *corev1.ConfigMap {
	ann := map[string]string{}
	if terminal {
		ann[annTerminal] = "true"
	}
	if !completion.IsZero() {
		ann[annCompletion] = completion.Format(time.RFC3339Nano)
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: ann}}
}

func withDeletion(cm *corev1.ConfigMap, at time.Time) *corev1.ConfigMap {
	t := metav1.NewTime(at)
	cm.DeletionTimestamp = &t
	cm.Finalizers = []string{"test/finalizer"} // required for a non-nil DeletionTimestamp to be valid
	return cm
}

func withIndex(cm *corev1.ConfigMap, idx string) *corev1.ConfigMap {
	if cm.Annotations == nil {
		cm.Annotations = map[string]string{}
	}
	cm.Annotations[annIndex] = idx
	return cm
}

var (
	testIsCandidate IsCandidate = func(o client.Object) bool {
		return o.GetAnnotations()[annTerminal] == "true"
	}
	testAgeTime AgeTimeFunc = func(o client.Object) time.Time {
		v := o.GetAnnotations()[annCompletion]
		if v == "" {
			return time.Time{}
		}
		t, _ := time.Parse(time.RFC3339Nano, v)
		return t
	}
	testIndex IndexFunc = func(o client.Object) string {
		return o.GetAnnotations()[annIndex]
	}
)

func names(objs []client.Object) []string {
	out := make([]string, 0, len(objs))
	for _, o := range objs {
		out = append(out, o.GetName())
	}
	return out
}

func TestDefaultFilter_TerminalAndAge(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour

	tests := []struct {
		name     string
		obj      *corev1.ConfigMap
		wantReap bool
	}{
		{
			name:     "terminal, older than TTL → reaped",
			obj:      obj("old", true, now.Add(-2*time.Hour)),
			wantReap: true,
		},
		{
			name:     "terminal, younger than TTL → retained",
			obj:      obj("young", true, now.Add(-30*time.Minute)),
			wantReap: false,
		},
		{
			name:     "non-terminal, even if ancient → never reaped",
			obj:      obj("pending", false, now.Add(-100*time.Hour)),
			wantReap: false,
		},
		{
			name:     "terminal but terminating (DeletionTimestamp) → skipped",
			obj:      withDeletion(obj("deleting", true, now.Add(-2*time.Hour)), now.Add(-time.Minute)),
			wantReap: false,
		},
		{
			name:     "terminal but no completion time (age 0) → never reaped (fail-safe)",
			obj:      obj("no-ct", true, time.Time{}),
			wantReap: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DefaultFilter([]client.Object{tt.obj}, testIsCandidate, ttl, testAgeTime, nil, 0, now)
			if tt.wantReap {
				assert.Equal(t, []string{tt.obj.Name}, names(got))
			} else {
				assert.Empty(t, got)
			}
		})
	}
}

// TestDefaultFilter_TTLBoundary pins the strict inequality: age must EXCEED ttl (now - ct > ttl), so an
// object exactly at the TTL boundary is retained and one a second past it is reaped.
func TestDefaultFilter_TTLBoundary(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour

	exactlyTTL := obj("exactly-ttl", true, now.Add(-ttl))         // age == ttl → not > ttl → retained
	justOver := obj("just-over", true, now.Add(-ttl-time.Second)) // age == ttl+1s → reaped

	got := DefaultFilter([]client.Object{exactlyTTL, justOver}, testIsCandidate, ttl, testAgeTime, nil, 0, now)
	assert.Equal(t, []string{"just-over"}, names(got), "only the object strictly older than TTL is reaped")
}

// TestDefaultFilter_NoCapByDefault verifies maxCount<=0 disables the per-index cap: only expired objects
// are returned regardless of how many non-expired ones share an index (storage-foundation passes 0).
func TestDefaultFilter_NoCapByDefault(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour

	var objs []client.Object
	for _, n := range []string{"a", "b", "c", "d"} {
		objs = append(objs, withIndex(obj(n, true, now.Add(-10*time.Minute)), "same")) // all young, same index
	}

	got := DefaultFilter(objs, testIsCandidate, ttl, testAgeTime, testIndex, 0, now)
	assert.Empty(t, got, "maxCount=0 must not cap retained objects")
}

// TestDefaultFilter_CapOverflow exercises the optional per-index cap (maxCount>0): the youngest maxCount
// per index are retained and the rest overflow into the delete set, on top of the expired ones.
func TestDefaultFilter_CapOverflow(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	ttl := time.Hour

	// Four young (non-expired) terminal objects in one index; cap=2 keeps the 2 youngest, overflows 2.
	young := []client.Object{
		withIndex(obj("y1", true, now.Add(-10*time.Minute)), "idx"),
		withIndex(obj("y2", true, now.Add(-20*time.Minute)), "idx"),
		withIndex(obj("y3", true, now.Add(-30*time.Minute)), "idx"),
		withIndex(obj("y4", true, now.Add(-40*time.Minute)), "idx"),
	}
	got := DefaultFilter(young, testIsCandidate, ttl, testAgeTime, testIndex, 2, now)
	// The two oldest of the young set overflow the cap.
	assert.ElementsMatch(t, []string{"y3", "y4"}, names(got))
}

func TestGetAge(t *testing.T) {
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)

	assert.Equal(t, time.Duration(0), getAge(obj("zero", true, time.Time{}), testAgeTime, now),
		"zero completion time yields age 0 (fail-safe)")
	assert.Equal(t, 90*time.Minute, getAge(obj("ninety", true, now.Add(-90*time.Minute)), testAgeTime, now))
}

func TestIsTerminating(t *testing.T) {
	now := time.Now()
	assert.False(t, isTerminating(obj("live", true, now)))
	assert.True(t, isTerminating(withDeletion(obj("dying", true, now), now)))
}
