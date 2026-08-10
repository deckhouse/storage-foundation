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

package controllers

import (
	"fmt"
	"time"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	"k8s.io/apimachinery/pkg/util/duration"

	storagev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

// Observable markers on a VolumeSnapshotContent. Values mirror external-snapshotter v8.5.0
// (pkg/utils/util.go); they are duplicated as literals instead of importing the sidecar module,
// which is not a dependency of this controller.
const (
	// snapshotContentFinalizer is added by the cluster-wide snapshot-controller. In VSC-only mode
	// that controller only adds the finalizer and returns, so the finalizer proves the snapshot
	// stack is alive — it says nothing about the per-driver sidecar having started any work.
	snapshotContentFinalizer = "snapshot.storage.kubernetes.io/volumesnapshotcontent-bound-protection"
	// annVolumeSnapshotBeingCreated is written by the per-driver sidecar with a real API patch
	// strictly before the CSI CreateSnapshot call, and is deliberately kept across timeouts and
	// transient errors. Its presence means "the request was sent and/or is being retried", NOT
	// "an RPC is in flight right now".
	annVolumeSnapshotBeingCreated = "snapshot.storage.kubernetes.io/volumesnapshot-being-created"
)

// Default diagnostic thresholds. These are intentionally NOT exposed through ModuleConfig: they
// do not affect safety (no terminal decision is derived from time), and useful values are not yet
// known from real clusters. A wrong value produces premature or late diagnostics, never a failed
// request.
const (
	defaultNoPickupGrace     = 2 * time.Minute
	defaultNoCompletionGrace = 2 * time.Hour
)

// Requeue pacing for a content that has not produced a result yet. The controller now watches
// VolumeSnapshotContent, so every meaningful change wakes the request immediately and the periodic
// requeue is only a safety net for a missed or filtered event. The ceiling stays deliberately low:
// it is raised only as far as the watch is proven to cover, and one minute is already an order of
// magnitude less API traffic than the previous fixed five seconds.
const (
	snapshotPollInitialInterval = 5 * time.Second
	snapshotPollMaxInterval     = time.Minute
	// snapshotPollBackoffAfter is the age at which the interval starts doubling.
	snapshotPollBackoffAfter = 30 * time.Second
)

// snapshotPollInterval derives the safety-net requeue delay from the age of the content.
//
// It is derived, not accumulated: backoff state in memory would be lost on every restart and
// leader change, and would silently differ between replicas. Note the distinction from the
// diagnosis itself — pacing may be approximate, the diagnosis may not, which is why the classifier
// is kept memoryless for a different and stronger reason.
func snapshotPollInterval(age time.Duration) time.Duration {
	interval := snapshotPollInitialInterval
	for threshold := snapshotPollBackoffAfter; age >= threshold; threshold *= 2 {
		interval *= 2
		if interval >= snapshotPollMaxInterval {
			return snapshotPollMaxInterval
		}
	}
	return interval
}

// stallThresholds carries the diagnostic grace periods so tests can use millisecond-scale values.
type stallThresholds struct {
	// NoPickup applies while nothing has visibly picked the request up.
	NoPickup time.Duration
	// NoCompletion applies once the executor has visibly started, and is necessarily much larger:
	// snapshotting a large volume is legitimately slow.
	NoCompletion time.Duration
}

func defaultStallThresholds() stallThresholds {
	return stallThresholds{
		NoPickup:     defaultNoPickupGrace,
		NoCompletion: defaultNoCompletionGrace,
	}
}

// stallDiagnosis is the classifier output. An empty Reason with Stalled=false means "nothing to
// report": the request is progressing normally, finished, or is in a state owned by another track.
type stallDiagnosis struct {
	Stalled bool
	Reason  string
	Message string
}

// classifyVSC is a finite state classifier: it derives a stall diagnosis from the currently
// observable state of a VolumeSnapshotContent and nothing else.
//
// Allowed inputs — and this list is closed on purpose:
//   - presence of the cluster snapshot-controller finalizer;
//   - status: absent / error / readyToUse;
//   - presence of the being-created annotation;
//   - age = now - creationTimestamp;
//   - thresholds.
//
// It must never gain memory: no previous condition, no previous reason, no reconcile or retry
// counters, no cached history. The moment it has memory, the state table stops being its complete
// description and behaviour is defined by accumulated state instead of the documented contract.
// Comparing against the previously reported diagnosis is the caller's responsibility.
//
// Time yields a diagnosis, never a terminal verdict: a request is never failed because it took
// too long (aborting an in-flight CSI call risks a duplicate backend snapshot).
//
// Evaluation order refines the state table for combinations it does not enumerate: a written
// result (readyToUse or error) proves the stack worked, so it outranks the finalizer check and
// cannot be misreported as SnapshotStackUnavailable. The no-pickup grace gates every absence,
// including a missing finalizer, because being young is not evidence of anything being wrong.
func classifyVSC(vsc *snapshotv1.VolumeSnapshotContent, now time.Time, thresholds stallThresholds) stallDiagnosis {
	if vsc == nil {
		return stallDiagnosis{}
	}

	if vscIsReadyToUse(vsc) {
		return stallDiagnosis{}
	}

	// A CSI error is classified by the VCR-contract track, not here: the sidecar retries without a
	// cap and clears the error on success. The message is surfaced by the existing pending path.
	if vscHasError(vsc) {
		return stallDiagnosis{}
	}

	// Every threshold is measured from creationTimestamp, so without it there is no age to reason
	// about. A real API server always sets it; anything else is a fabricated object, and inventing
	// an age from the zero time would report a stall of several centuries.
	if vsc.CreationTimestamp.IsZero() {
		return stallDiagnosis{}
	}
	age := now.Sub(vsc.CreationTimestamp.Time)

	// Nothing is diagnosed about a content younger than the no-pickup grace, whichever marker is
	// missing. Observed on a dev cluster: the VolumeSnapshotContent watch wakes this reconcile on the
	// content's own creation event, so the first classification runs at an age of about one second —
	// before the cluster snapshot-controller has had any chance to add its finalizer. Without this
	// gate every healthy capture reported "the snapshot stack appears not to be running", emitted a
	// Warning event, and then cleared it, which also left a permanent Stalled=False condition on
	// requests that never stalled.
	if age < thresholds.NoPickup {
		return stallDiagnosis{}
	}

	if !vscHasSnapshotControllerFinalizer(vsc) {
		return stallDiagnosis{
			Stalled: true,
			Reason:  storagev1alpha1.ConditionReasonSnapshotStackUnavailable,
			Message: fmt.Sprintf(
				"the cluster snapshot-controller has not added its finalizer to VolumeSnapshotContent %q "+
					"(CSI driver %q, age %s): the snapshot stack appears not to be running",
				vsc.Name, vsc.Spec.Driver, humanAge(age)),
		}
	}

	if vscHasBeingCreatedAnnotation(vsc) {
		if age < thresholds.NoCompletion {
			return stallDiagnosis{}
		}
		// Deliberately phrased as "has existed for" and not "the RPC has been running for": the
		// annotation carries no timestamp, so its own age is unknown (see design §6.2).
		return stallDiagnosis{
			Stalled: true,
			Reason:  storagev1alpha1.ConditionReasonSnapshotExecutionNotCompleting,
			Message: fmt.Sprintf(
				"VolumeSnapshotContent %q has existed for %s and snapshot execution is still marked as "+
					"being created (CSI driver %q): the request was sent to the storage system or is being "+
					"retried, but no result has been written",
				vsc.Name, humanAge(age), vsc.Spec.Driver),
		}
	}

	return stallDiagnosis{
		Stalled: true,
		Reason:  storagev1alpha1.ConditionReasonSnapshotExecutionUnobservable,
		Message: fmt.Sprintf(
			"no observable snapshot execution for VolumeSnapshotContent %q (CSI driver %q) for %s: "+
				"snapshot-controller finalizer present, no status written, being-created annotation absent",
			vsc.Name, vsc.Spec.Driver, humanAge(age)),
	}
}

func vscHasSnapshotControllerFinalizer(vsc *snapshotv1.VolumeSnapshotContent) bool {
	for _, f := range vsc.Finalizers {
		if f == snapshotContentFinalizer {
			return true
		}
	}
	return false
}

func vscHasBeingCreatedAnnotation(vsc *snapshotv1.VolumeSnapshotContent) bool {
	_, ok := vsc.Annotations[annVolumeSnapshotBeingCreated]
	return ok
}

func vscIsReadyToUse(vsc *snapshotv1.VolumeSnapshotContent) bool {
	return vsc.Status != nil && vsc.Status.ReadyToUse != nil && *vsc.Status.ReadyToUse
}

// vscHasError mirrors the check already used by the snapshot reconcile path: an empty message is
// treated as no error.
func vscHasError(vsc *snapshotv1.VolumeSnapshotContent) bool {
	return vsc.Status != nil && vsc.Status.Error != nil &&
		vsc.Status.Error.Message != nil && *vsc.Status.Error.Message != ""
}

// humanAge keeps condition messages stable across reconciles of the same second and avoids
// sub-second noise in an operator-facing text.
func humanAge(age time.Duration) string {
	if age < 0 {
		age = 0
	}
	return duration.HumanDuration(age)
}
