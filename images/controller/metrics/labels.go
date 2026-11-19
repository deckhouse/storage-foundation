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

package metrics

import "time"

// Label names. The same constants name the labels at registration and key them at every write.
const (
	labelMode         = "mode"
	labelDriver       = "driver"
	labelStorageClass = "storage_class"
	labelState        = "state"
	labelResult       = "result"
	labelAge          = "age"
	labelStage        = "stage"
)

// labelUnknown replaces a label value that is not one of the known ones.
const labelUnknown = "unknown"

// Values of the state label.
const (
	stateCompleted = "completed"
	stateFailed    = "failed"
	statePending   = "pending"
)

// Values of the age label.
const (
	ageLT3m   = "lt_3m"
	age3m15m  = "3m_15m"
	age15m30m = "15m_30m"
	age30m2h  = "30m_2h"
	age2h24h  = "2h_24h"
	ageGT24h  = "gt_24h"
)

// Values of the stage label.
const (
	stageNotStarted         = "not_started"
	stageSnapshotInProgress = "snapshot_in_progress"
	stageOther              = "other"
)

// ageBucket returns the age label for the time from created to now, the youngest one if created is after now.
func ageBucket(created, now time.Time) string {
	age := now.Sub(created)

	switch {
	case age < 3*time.Minute:
		return ageLT3m
	case age < 15*time.Minute:
		return age3m15m
	case age < 30*time.Minute:
		return age15m30m
	case age < 2*time.Hour:
		return age30m2h
	case age < 24*time.Hour:
		return age2h24h
	}

	return ageGT24h
}
