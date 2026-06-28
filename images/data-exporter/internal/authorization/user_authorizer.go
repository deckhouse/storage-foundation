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

package authorization

import (
	"context"

	"github.com/deckhouse/storage-foundation/common"
)

//go:generate go tool mockgen -typed -package mock -copyright_file ../../../../hack/boilerplate.txt -write_source_comment -destination=./../mock/$GOFILE -source=$GOFILE
type UserAuthorizer interface {
	// AuthenticateUser(ctx context.Context, authData dev1alpha1.AuthData) (authenticated bool, username string, groups []string, err error)
	AuthenticateUserByToken(ctx context.Context, token string) (authenticated bool, username string, groups []string, err error)
	AuthorizeUser(ctx context.Context, operation common.Operation, namespace, username string, groups []string) (allowed bool, reason string, err error)
}
