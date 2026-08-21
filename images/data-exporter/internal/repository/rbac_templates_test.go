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

package repository

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

// The permission a client of a running transfer needs is decided in two places that cannot see each
// other: the SubjectAccessReview built by AuthorizeUser, and the ClusterRole shipped in the chart. A
// mismatch between them is invisible until a real cluster answers 403, so the tests below build the
// expected rules of the role out of the very constants the authorization code uses. They compare
// NAMES only — that a subject holding the role passes the SubjectAccessReview is a property of the
// API server and is covered by an end-to-end run, not here.
const (
	// repoRoot is this repository's root as seen from images/data-exporter/internal/repository.
	repoRoot = "../../../.."

	// rbacV2Dir holds the roles a cluster administrator grants to people and service accounts.
	rbacV2Dir = "templates/rbacv2"

	// transferVolumeDataRoleName is spelled out here on purpose. It is a public name that
	// documentation and RoleBindings outside this repository refer to, so it is part of what the
	// module promises and cannot be changed to suit a refactoring.
	transferVolumeDataRoleName = "d8:use:capability:module:storage-foundation:transfer_volume_data"

	// aggregationLabelPrefix marks a role whose rules are folded into a cluster-wide access level,
	// reaching every subject of that level without anyone binding the role.
	aggregationLabelPrefix = "rbac.deckhouse.io/aggregate-to-"
)

// authorizedSubresources returns the "<resource>/<subresource>" pairs the data-exporter authorizes
// against, one per transfer direction. Enumerated from AuthzResources rather than written out, so a
// direction added to the authorization code appears here on its own.
func authorizedSubresources() []string {
	resources := AuthzResources()
	pairs := make([]string, 0, len(resources))
	for _, resource := range resources {
		pairs = append(pairs, resource+"/"+AuthzSubresource)
	}

	return pairs
}

// loadRBACv2ClusterRoles parses every ClusterRole under templates/rbacv2 and reports how much was
// read, so that "nothing violates the rule" can be told apart from "nothing was examined".
//
// Limit: the files are read as plain YAML. That is sound only because this part of the chart carries
// no Helm actions today (unlike, say, templates/user-authz-cluster-roles.yaml, which does). Should a
// role here gain templating, this helper must learn to render the chart — narrowing its scope to the
// files that still parse would quietly shrink the guard to nothing.
func loadRBACv2ClusterRoles(t *testing.T) (roles []rbacv1.ClusterRole, files, documents int) {
	t.Helper()

	dir := filepath.Join(repoRoot, rbacV2Dir)
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}
		files++

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(raw)))
		for {
			document, err := reader.Read()
			if errors.Is(err, io.EOF) {
				return nil
			}
			require.NoErrorf(t, err, "splitting %s into YAML documents", path)
			if len(bytes.TrimSpace(document)) == 0 {
				continue
			}
			documents++

			var role rbacv1.ClusterRole
			require.NoErrorf(t, yaml.Unmarshal(document, &role), "parsing %s as a ClusterRole", path)
			if role.Kind != "ClusterRole" {
				continue
			}
			roles = append(roles, role)
		}
	})
	require.NoErrorf(t, err, "walking %s", dir)

	return roles, files, documents
}

// findClusterRole returns the role with the given name, failing the test when it is absent.
func findClusterRole(t *testing.T, roles []rbacv1.ClusterRole, name string) rbacv1.ClusterRole {
	t.Helper()

	for _, role := range roles {
		if role.Name == name {
			return role
		}
	}
	require.FailNowf(t, "ClusterRole not found", "%q is not defined under %s", name, rbacV2Dir)

	return rbacv1.ClusterRole{}
}

// grantedSubresources returns every "<resource>/<subresource>" the role grants AuthzVerb on within
// AuthzAPIGroup. Rules naming a bare resource are ignored: creating a DataExport or a DataImport is a
// separate permission from talking to the running transfer, and only the latter is what
// AuthorizeUser checks.
//
// A wildcard or a resourceNames restriction in an inspected rule fails the test outright: either one
// would make a plain name comparison meaningless — a wildcard grants far more than the pairs, a
// resourceNames list far less — and a guard that answers the wrong question is worse than none.
func grantedSubresources(t *testing.T, role rbacv1.ClusterRole) []string {
	t.Helper()

	var granted []string
	for i, rule := range role.Rules {
		require.NotContainsf(t, rule.APIGroups, rbacv1.APIGroupAll,
			"rule %d of %q matches every API group by wildcard", i, role.Name)
		require.NotContainsf(t, rule.Verbs, rbacv1.VerbAll,
			"rule %d of %q matches every verb by wildcard", i, role.Name)

		if !slices.Contains(rule.APIGroups, AuthzAPIGroup) || !slices.Contains(rule.Verbs, AuthzVerb) {
			continue
		}
		require.NotContainsf(t, rule.Resources, rbacv1.ResourceAll,
			"rule %d of %q matches every resource by wildcard", i, role.Name)

		for _, resource := range rule.Resources {
			if !strings.Contains(resource, "/") {
				continue
			}
			require.Emptyf(t, rule.ResourceNames,
				"rule %d of %q restricts %s to named objects, so it does not grant the subresource in general",
				i, role.Name, resource)
			granted = append(granted, resource)
		}
	}

	return granted
}

// TestTransferVolumeDataRoleGrantsAuthorizedSubresources is the link between the authorization code
// and the shipped role: the role must grant AuthzVerb on exactly the pairs AuthorizeUser asks the API
// server about — no fewer (the client gets 403 on a direction the role forgot) and no more (the role
// hands out a permission nothing checks).
func TestTransferVolumeDataRoleGrantsAuthorizedSubresources(t *testing.T) {
	roles, files, documents := loadRBACv2ClusterRoles(t)
	t.Logf("read %d YAML file(s) and %d document(s) under %s, %d ClusterRole(s)",
		files, documents, rbacV2Dir, len(roles))
	require.NotEmpty(t, roles, "no ClusterRole was read, so this check compared nothing")

	role := findClusterRole(t, roles, transferVolumeDataRoleName)

	want := authorizedSubresources()
	require.NotEmpty(t, want, "no transfer direction is authorized, so this check compared nothing")

	assert.ElementsMatch(t, want, grantedSubresources(t, role),
		"%q must grant %q on exactly the subresources AuthorizeUser authorizes against",
		transferVolumeDataRoleName, AuthzVerb)
}

// TestNoAggregatedRoleGrantsVolumeDataTransfer keeps the permission behind an explicit binding.
// Moving a volume out of the cluster and back in is granted to a named subject on purpose; an
// aggregation label on any role carrying these subresources would hand the same reach to every
// subject of a cluster-wide access level, silently and without a binding anyone can point at.
//
// Limit: it inspects the roles under templates/rbacv2 only. A role defined elsewhere in the chart, or
// written by a cluster administrator, is outside its reach.
func TestNoAggregatedRoleGrantsVolumeDataTransfer(t *testing.T) {
	roles, files, documents := loadRBACv2ClusterRoles(t)
	t.Logf("read %d YAML file(s) and %d document(s) under %s, %d ClusterRole(s)",
		files, documents, rbacV2Dir, len(roles))

	pairs := authorizedSubresources()
	require.NotEmpty(t, pairs, "no transfer direction is authorized, so this check compared nothing")

	aggregated := 0
	for _, role := range roles {
		isAggregated := false
		for label := range role.Labels {
			if strings.HasPrefix(label, aggregationLabelPrefix) {
				isAggregated = true

				break
			}
		}
		if !isAggregated {
			continue
		}
		aggregated++

		granted := grantedSubresources(t, role)
		for _, pair := range pairs {
			assert.NotContainsf(t, granted, pair,
				"%q is aggregated into a cluster-wide access level and must not grant %q on %s",
				role.Name, AuthzVerb, pair)
		}
	}
	t.Logf("inspected %d aggregated ClusterRole(s) out of %d", aggregated, len(roles))
	require.NotZero(t, aggregated, "no aggregated ClusterRole was inspected, so this check compared nothing")
}
