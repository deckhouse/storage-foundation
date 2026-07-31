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
	"testing"

	"github.com/stretchr/testify/assert"

	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

const (
	reconcileDataExportName      = "test-de"
	reconcileDataExportNamespace = "user-ns"
	userPVCName                  = "user-pvc"
	anotherDataExportName        = "test-de-2"
	anotherDataExportNamespace   = "user-ns-2"
)

// TestGeneratedNames_DependOnlyOnTheOwnerIdentity pins the property the orphan sweep rests on. The sweep
// rebuilds the names of resources it did not create, from a record left on a volume, and the only
// identity that record can be trusted to carry is the owner's. If a generated name ever started depending
// on what was exported, a VirtualDisk export would be the first casualty: the claim the volume goes back
// to is the disk's backing PVC, under a name nothing else knows.
func TestGeneratedNames_DependOnlyOnTheOwnerIdentity(t *testing.T) {
	fromDisk := NewNamesFromShort(dev1alpha1.KindVirtualDiskShort, "disk-a", reconcileDataExportNamespace, reconcileDataExportName)
	fromBackingClaim := NewNamesFromShort(dev1alpha1.KindVirtualDiskShort, "disk-a-pvc-9f2c1", reconcileDataExportNamespace, reconcileDataExportName)

	fromDisk.TargetName, fromBackingClaim.TargetName = "", ""
	assert.Equal(t, fromDisk, fromBackingClaim,
		"what was exported must not reach any generated name; only the owner's namespace and name may")

	// And the sweep's constructor reproduces exactly those names from the recorded suffix alone.
	assert.Equal(t, fromDisk, NewNamesFromPersistedIdentity(fromDisk.TargetKindShort, fromDisk.HashSuffix))
}

func TestValidateHashAndTarget(t *testing.T) {
	generatedNames := NewNames(dev1alpha1.KindPVC, userPVCName, reconcileDataExportNamespace, reconcileDataExportName)
	generatedNames2 := NewNames(dev1alpha1.KindPVC, userPVCName, anotherDataExportNamespace, anotherDataExportName)

	tests := []struct {
		name                string
		targetKindShort     string
		hashSuffix          string
		dataExportNamespace string
		dataExportName      string
		wantErr             bool
	}{
		{
			name:                "Valid hash and target",
			targetKindShort:     dev1alpha1.KindPVCShort,
			hashSuffix:          generatedNames.HashSuffix,
			dataExportNamespace: reconcileDataExportNamespace,
			dataExportName:      reconcileDataExportName,
			wantErr:             false,
		},
		{
			name:                "Invalid hash suffix",
			targetKindShort:     dev1alpha1.KindPVCShort,
			hashSuffix:          "invalid-hash-suffix",
			dataExportNamespace: reconcileDataExportNamespace,
			dataExportName:      reconcileDataExportName,
			wantErr:             true,
		},
		{
			name:                "Invalid target kind",
			targetKindShort:     "unknown kind",
			hashSuffix:          generatedNames.HashSuffix,
			dataExportNamespace: reconcileDataExportNamespace,
			dataExportName:      reconcileDataExportName,
			wantErr:             true,
		},
		{
			name:                "Invalid hash suffix from another dataExport",
			targetKindShort:     dev1alpha1.KindPVCShort,
			hashSuffix:          generatedNames2.HashSuffix,
			dataExportNamespace: reconcileDataExportNamespace,
			dataExportName:      reconcileDataExportName,
			wantErr:             true,
		},
		{
			name:                "Valid hash suffix from another dataExport",
			targetKindShort:     dev1alpha1.KindPVCShort,
			hashSuffix:          generatedNames2.HashSuffix,
			dataExportNamespace: anotherDataExportNamespace,
			dataExportName:      anotherDataExportName,
			wantErr:             false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateHashAndTarget(tt.targetKindShort, tt.hashSuffix, tt.dataExportNamespace, tt.dataExportName)

			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}
}
