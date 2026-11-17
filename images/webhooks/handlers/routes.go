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

package handlers

import (
	"fmt"
	"net/http"

	"github.com/deckhouse/sds-common-lib/logger"
	"github.com/deckhouse/sds-common-lib/logger/kwhlogger"
	snapv1 "github.com/deckhouse/storage-foundation/api/snapshot/v1"
	dev1alpha1 "github.com/deckhouse/storage-foundation/api/v1alpha1"
)

const (
	VolumeSnapshotMutatorID = "volumeSnapshotMutation"
	DataExportValidatorID   = "dataExportValidation"
)

func Handlers(l logger.Logger) (map[string]http.Handler, error) {
	SetLogger(l)
	kwhLog := kwhlogger.New(l)

	vs, err := GetMutatingWebhookHandler(VolumeSnapshotMutate, VolumeSnapshotMutatorID, new(snapv1.VolumeSnapshot), kwhLog)
	if err != nil {
		return nil, fmt.Errorf("creating the VolumeSnapshot mutator: %w", err)
	}
	de, err := NewValidatingWebhookHandler(DataExportValidateFunc(), DataExportValidatorID, &dev1alpha1.DataExport{}, kwhLog)
	if err != nil {
		return nil, fmt.Errorf("creating the DataExport validator: %w", err)
	}
	return map[string]http.Handler{
		"/volume-snapshot-mutate": vs,
		"/dataexport-validate":    de,
	}, nil
}
