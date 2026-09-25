/*
Copyright 2026 Flant JSC

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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMakeVolumeList_ReadOnlyFollowsTheKindOfVolume is the guard for the defect: the export PVC was mounted
// read-only unconditionally. For a live-volume export that is the guarantee an export cannot alter what it
// exports, and it stays. For a snapshot export the volume is a fresh restore that may carry a dirty
// journal, and a driver that has to replay it -- DRBD/replicated among them -- cannot mount it read-only at
// all, so the exporter pod never started.
func TestMakeVolumeList_ReadOnlyFollowsTheKindOfVolume(t *testing.T) {
	t.Run("live volume is mounted read-only", func(t *testing.T) {
		vols := makeVolumeList("export-pvc", true)
		require.Len(t, vols, 1)
		require.NotNil(t, vols[0].PersistentVolumeClaim)
		assert.True(t, vols[0].PersistentVolumeClaim.ReadOnly,
			"an export must not be able to alter the user's own volume")
		assert.Equal(t, "export-pvc", vols[0].PersistentVolumeClaim.ClaimName)
	})

	t.Run("snapshot restore is mounted read-write", func(t *testing.T) {
		vols := makeVolumeList("export-pvc", false)
		require.Len(t, vols, 1)
		require.NotNil(t, vols[0].PersistentVolumeClaim)
		assert.False(t, vols[0].PersistentVolumeClaim.ReadOnly,
			"a freshly restored filesystem may need its journal replayed, which a read-only mount forbids")
	})
}
