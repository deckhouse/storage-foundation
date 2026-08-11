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

package dataimport

// Status of a target reference
type TargetStatus int

const (
	TargetStatusUnknown      TargetStatus = iota // Unable to determine target status
	TargetStatusPending                          // Target is being initialized
	TargetStatusNeedConsumer                     // Consumer is needed to initialize target
	TargetStatusReady                            // Target is ready for use
	TargetStatusFailed                           // Target  failed
)
