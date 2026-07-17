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

package ttl_control

import (
	"context"
	"time"

	"github.com/deckhouse/storage-foundation/common"
)

//go:generate go tool mockgen -typed -package mock -copyright_file ../../../../hack/boilerplate.txt -write_source_comment -destination=./../mock/$GOFILE -source=$GOFILE

type DataManagerAccessTimestampUpdater interface {
	UpdateAccessTimestamp(ctx context.Context, operation common.Operation, resourceNamespace, resourceName string, timestamp time.Time) error
}

type DataManagerServerStateSetter interface {
	SetServerState(ctx context.Context, operation common.Operation, resourceNamespace, resourceName string, state common.ServerState) error
}
