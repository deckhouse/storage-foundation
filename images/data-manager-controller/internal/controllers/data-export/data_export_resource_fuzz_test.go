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

package dataexport

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	"github.com/deckhouse/storage-foundation/common"
)

// FuzzParsePVRecoveryInfo exercises parsePVRecoveryInfo against arbitrary PV
// annotation/label content. The controller writes these annotations itself,
// but a PV can drift from what it expects (manual edits, partial patches,
// restore from an old backup, races during upgrades), so recovery parsing
// must never panic and must only accept content it recognizes as its own.
func FuzzParsePVRecoveryInfo(f *testing.F) {
	const deNamespace = "de-ns"
	const deName = "de-name"

	validHash := common.NewNames(dev1alpha1.KindPVC, "user-pvc", deNamespace, deName).HashSuffix

	// self-consistent, valid recovery annotations for every known target kind
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("", string(corev1.PersistentVolumeReclaimDelete), "true", deName, deNamespace, deNamespace, dev1alpha1.KindVolumeSnapshotShort, validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRecycle), "true", deName, deNamespace, deNamespace, dev1alpha1.KindVirtualDiskShort, validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, dev1alpha1.KindVirtualDiskSnapshotShort, validHash)
	// invalid/empty reclaim policy
	f.Add("user-pvc", "", "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("user-pvc", "Whatever", "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("user-pvc", strings.ToUpper(string(corev1.PersistentVolumeReclaimRetain)), "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	// missing/incorrect data-exporter label
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "false", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "TRUE", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	// DataExport name/namespace mismatch
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", "other-de", deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, "other-ns", deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, "other-ns", dev1alpha1.KindPVCShort, validHash)
	// invalid target kind / hash suffix
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, "unknown", validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVC, validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, "")
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, "not-a-real-hash")
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash+"x")
	// everything empty
	f.Add("", "", "", "", "", "", "", "")
	// long / unicode / control-character values
	f.Add(strings.Repeat("a", 300), string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", strings.Repeat("d", 300), deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, strings.Repeat("k", 300), validHash)
	f.Add("user-pvc", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, strings.Repeat("h", 300))
	f.Add(" abcdйцукен///⚺⪣⾓␜ⅷⱦ⣠ⵉ⾛⃚⴦ⶭ", string(corev1.PersistentVolumeReclaimRetain), "true", deName, deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)
	f.Add("user-pvc\x00trailing", string(corev1.PersistentVolumeReclaimRetain)+"\x00", "true", deName+"\n", deNamespace, deNamespace, dev1alpha1.KindPVCShort, validHash)

	f.Fuzz(func(t *testing.T, userPVCName, originalReclaimPolicy, labelValue, dataExportName, dataExportNamespace, userPVCNamespace, targetKindShort, hashSuffix string) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: "fuzz-pv",
				Annotations: map[string]string{
					dev1alpha1.AnnotationUserPVCNameKey:             userPVCName,
					dev1alpha1.AnnotationOriginalReclaimPolicyKey:   originalReclaimPolicy,
					dev1alpha1.AnnotationStorageManagerNameKey:      dataExportName,
					dev1alpha1.AnnotationStorageManagerNamespaceKey: dataExportNamespace,
					dev1alpha1.AnnotationUserPVCNamespaceKey:        userPVCNamespace,
					dev1alpha1.AnnotationPVTargetKindShortKey:       targetKindShort,
					dev1alpha1.AnnotationPVHashSuffixKey:            hashSuffix,
				},
				Labels: map[string]string{
					dev1alpha1.LabelPVDataExporter: labelValue,
				},
			},
		}

		info, err := parsePVRecoveryInfo(pv, deNamespace, deName)
		if err != nil {
			if info != nil {
				t.Fatalf("parsePVRecoveryInfo returned a non-nil result alongside an error: %v", err)
			}
			return
		}

		if info == nil {
			t.Fatal("parsePVRecoveryInfo returned no error but a nil result")
		}

		switch info.TargetKindShort {
		case dev1alpha1.KindPVCShort, dev1alpha1.KindVolumeSnapshotShort, dev1alpha1.KindVirtualDiskShort, dev1alpha1.KindVirtualDiskSnapshotShort:
		default:
			t.Fatalf("parsePVRecoveryInfo accepted an unrecognized target kind: %q", info.TargetKindShort)
		}

		switch info.OriginalReclaimPolicy {
		case corev1.PersistentVolumeReclaimRetain, corev1.PersistentVolumeReclaimDelete, corev1.PersistentVolumeReclaimRecycle:
		default:
			t.Fatalf("parsePVRecoveryInfo accepted an invalid reclaim policy: %q", info.OriginalReclaimPolicy)
		}

		if info.DataExportName != deName {
			t.Fatalf("parsePVRecoveryInfo accepted a mismatched DataExport name: got %q, want %q", info.DataExportName, deName)
		}
		if info.DataExportNamespace != deNamespace {
			t.Fatalf("parsePVRecoveryInfo accepted a mismatched DataExport namespace: got %q, want %q", info.DataExportNamespace, deNamespace)
		}
		if info.UserPVCNamespace != deNamespace {
			t.Fatalf("parsePVRecoveryInfo accepted a mismatched user PVC namespace: got %q, want %q", info.UserPVCNamespace, deNamespace)
		}

		expectedHash := common.NewNames(dev1alpha1.KindPVC, "irrelevant", deNamespace, deName).HashSuffix
		if info.HashSuffix != expectedHash {
			t.Fatalf("parsePVRecoveryInfo accepted a hash suffix that does not match (deNamespace, deName): got %q, want %q", info.HashSuffix, expectedHash)
		}
	})
}
