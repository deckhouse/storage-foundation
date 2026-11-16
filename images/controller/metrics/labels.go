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

// Label names. The same constants name the labels at registration and key them at every write.
const (
	labelMode         = "mode"
	labelDriver       = "driver"
	labelStorageClass = "storage_class"
	labelState        = "state"
)

// labelUnknown replaces a label value that is not one of the known ones.
const labelUnknown = "unknown"

// Values of the state label.
const (
	stateCompleted = "completed"
	stateFailed    = "failed"
	statePending   = "pending"
)
