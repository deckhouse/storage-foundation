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
	"context"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMapVolumeSnapshotContentToVCR(t *testing.T) {
	tests := []struct {
		name       string
		labels     map[string]string
		wantLen    int
		wantName   string
		wantNsName string
	}{
		{
			name: "labeled VSC maps to owning VCR",
			labels: map[string]string{
				LabelKeyVCRNameFull:      "test-vcr",
				LabelKeyVCRNamespaceFull: "ns1",
				LabelKeyVCRUIDFull:       "uid-1",
			},
			wantLen:    1,
			wantName:   "test-vcr",
			wantNsName: "ns1",
		},
		{
			name:    "VSC without labels maps to nothing",
			labels:  nil,
			wantLen: 0,
		},
		{
			name:    "VSC missing namespace label maps to nothing",
			labels:  map[string]string{LabelKeyVCRNameFull: "test-vcr"},
			wantLen: 0,
		},
		{
			name:    "VSC missing name label maps to nothing",
			labels:  map[string]string{LabelKeyVCRNamespaceFull: "ns1"},
			wantLen: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			vsc := &snapshotv1.VolumeSnapshotContent{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "snapshot-uid-1-abc",
					Labels: tc.labels,
				},
			}
			reqs := mapVolumeSnapshotContentToVCR(context.Background(), vsc)
			if len(reqs) != tc.wantLen {
				t.Fatalf("expected %d requests, got %d", tc.wantLen, len(reqs))
			}
			if tc.wantLen == 1 {
				if reqs[0].Name != tc.wantName || reqs[0].Namespace != tc.wantNsName {
					t.Fatalf("expected %s/%s, got %s/%s", tc.wantNsName, tc.wantName, reqs[0].Namespace, reqs[0].Name)
				}
			}
		})
	}
}
