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

package common

import (
	"crypto/sha256"
	"encoding/base32"
	"fmt"
	"strings"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
	virtv1alpha2 "github.com/deckhouse/virtualization/api/core/v1alpha2"
)

type Names struct {
	TargetName          string
	TargetKindShort     string
	HashSuffix          string
	DeployName          string
	CASecretName        string
	HeadlessServiceName string
	IngressResourceName string
	ExportPVCName       string // Export-specific
	DummyJobName        string // Import-specific
}

func generateHashSuffix(namespace, name string) string {
	suffixSum := sha256.Sum256([]byte(namespace + "\x00" + name))
	return strings.ToLower(
		base32.HexEncoding.WithPadding(base32.NoPadding).EncodeToString(suffixSum[:10]),
	)
}

func NewNames(targetKind, targetName, namespace, name string) Names {
	return NewNamesFromShort(getShortKind(targetKind), targetName, namespace, name)
}

// NewNamesFromShort builds the generated resource names from an already-resolved target short kind
// (pvc/vd/snap/...). DataExport (C6) resolves the short from the GroupKind targetRef directly via
// classifyTargetRef, so it calls this instead of NewNames; DataImport still goes through NewNames with a kind.
func NewNamesFromShort(targetKindShort, targetName, namespace, name string) Names {
	hashSuffix := generateHashSuffix(namespace, name)

	return Names{
		TargetName:          targetName,
		TargetKindShort:     targetKindShort,
		HashSuffix:          hashSuffix,
		DeployName:          fmt.Sprintf("deploy-for-%s-%s", targetKindShort, hashSuffix),
		CASecretName:        fmt.Sprintf("ca-secret-for-%s-%s", targetKindShort, hashSuffix),
		HeadlessServiceName: fmt.Sprintf("service-for-%s-%s", targetKindShort, hashSuffix),
		IngressResourceName: fmt.Sprintf("ingress-for-%s-%s", targetKindShort, hashSuffix),
		ExportPVCName:       fmt.Sprintf("pvc-for-%s-%s", targetKindShort, hashSuffix),
		DummyJobName:        fmt.Sprintf("dummy-for-%s-%s", targetKindShort, hashSuffix),
	}
}

func getShortKind(kind string) string {
	switch kind {
	case dev1alpha1.KindPVC:
		return dev1alpha1.KindPVCShort
	case virtv1alpha2.VirtualDiskKind:
		return dev1alpha1.KindVirtualDiskShort
	case dev1alpha1.KindVolumeSnapshot:
		return dev1alpha1.KindVolumeSnapshotShort
	case virtv1alpha2.VirtualDiskSnapshotKind:
		return dev1alpha1.KindVirtualDiskSnapshotShort
	default:
		return kind
	}
}

// DeployNameForHash and ExportPVCNameForHash are centralized name generators.
// They must match the naming pattern in NewNames to ensure orphan resource cleanup works correctly.
// If the naming pattern changes, update both NewNames and these functions together.

func DeployNameForHash(targetKindShort, hashSuffix string) string {
	return fmt.Sprintf("deploy-for-%s-%s", targetKindShort, hashSuffix)
}

func ExportPVCNameForHash(targetKindShort, hashSuffix string) string {
	return fmt.Sprintf("pvc-for-%s-%s", targetKindShort, hashSuffix)
}

func ValidateHashAndTarget(targetKindShort, hashSuffix, namespace, name string) error {
	if !isValidTargetKind(targetKindShort) {
		return fmt.Errorf("invalid targetKindShort: %q", targetKindShort)
	}

	if !isValidHashSuffix(hashSuffix, namespace, name) {
		return fmt.Errorf("invalid hashSuffix: %q", hashSuffix)
	}

	return nil
}

func isValidHashSuffix(hashSuffix, namespace, name string) bool {
	expectedHashSuffix := generateHashSuffix(namespace, name)
	return hashSuffix == expectedHashSuffix
}

func isValidTargetKind(targetKindShort string) bool {
	switch targetKindShort {
	case dev1alpha1.KindPVCShort,
		dev1alpha1.KindVirtualDiskShort,
		dev1alpha1.KindVolumeSnapshotShort,
		dev1alpha1.KindVirtualDiskSnapshotShort,
		dev1alpha1.KindSnapshotShort:
	default:
		return false
	}

	return true
}
