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

	// backupAgentAggregationLabel is the one axis under aggregationLabelPrefix that does NOT put the
	// rules of a role behind a cluster-wide access level, and therefore the one exemption from
	// TestNoAggregatedRoleGrantsVolumeDataTransfer. It is collected by a single ClusterRole,
	// d8:use:capability:module:state-snapshotter:backup_agent, which grants nothing until somebody
	// binds it: a role on this axis still reaches exactly the subjects of an explicit RoleBinding,
	// which is the property that ban exists to protect. Every other axis stays banned.
	backupAgentAggregationLabel = aggregationLabelPrefix + "backup-agent-as"

	// backupAgentAggregationValue is the value transferVolumeDataRoleName must carry on that axis. It
	// has to equal the value the aggregating role selects on, and nothing in this repository can check
	// that: a mismatch is silent, the role simply stays out of the aggregated set, and the transfer
	// then answers 403 to a subject whose binding looks correct.
	backupAgentAggregationValue = "member"
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

// aggregationLabels returns the "rbac.deckhouse.io/aggregate-to-*" labels of the role, keyed by
// label, so that a check can talk about the axes a role declares without restating the other labels.
func aggregationLabels(role rbacv1.ClusterRole) map[string]string {
	axes := make(map[string]string)
	for label, value := range role.Labels {
		if strings.HasPrefix(label, aggregationLabelPrefix) {
			axes[label] = value
		}
	}

	return axes
}

// TestTransferVolumeDataRoleAggregatesOnlyIntoBackupAgent pins which aggregation axes the role
// declares, and covers the half TestNoAggregatedRoleGrantsVolumeDataTransfer cannot: that test
// inspects only the roles it finds aggregated, so a label dropped, or given a value the aggregate does
// not select, leaves it green while the aggregated role silently loses the transfer permission — and
// the failure surfaces as a 403 in a cluster, on a binding that looks correct. A misspelling that
// keeps the aggregate-to- prefix is not in that gap: such a label counts as another axis, so the ban
// inspects the role and goes red on it.
//
// The set is compared exactly, not just searched for the expected axis. The kubernetes axis in
// particular must never appear here: with it, reading a volume out of the cluster and writing one
// back in would reach every subject of the corresponding cluster-wide access level, with no binding
// anyone can point at.
//
// Limit: the "only" in the name is about the aggregate-to-* axes and reaches no further. An
// aggregationRule selecting on heritage, module or rbac.deckhouse.io/kind alone would collect this
// role while it carries no aggregate-to-* label at all, and nothing here can see that — the selecting
// role is not part of this chart.
func TestTransferVolumeDataRoleAggregatesOnlyIntoBackupAgent(t *testing.T) {
	roles, files, documents := loadRBACv2ClusterRoles(t)
	t.Logf("read %d YAML file(s) and %d document(s) under %s, %d ClusterRole(s)",
		files, documents, rbacV2Dir, len(roles))
	require.NotEmpty(t, roles, "no ClusterRole was read, so this check compared nothing")

	role := findClusterRole(t, roles, transferVolumeDataRoleName)

	axes := aggregationLabels(role)
	t.Logf("%q declares %d aggregation label(s) among its %d label(s)", role.Name, len(axes), len(role.Labels))

	assert.Equal(t, map[string]string{backupAgentAggregationLabel: backupAgentAggregationValue}, axes,
		"%q must declare %s=%s and no other aggregation axis: that value is what puts the role into the "+
			"aggregated set, and any other axis would fold the transfer permission into a cluster-wide access level",
		role.Name, backupAgentAggregationLabel, backupAgentAggregationValue)
}

// TestBackupAgentAggregationIsCarriedOnlyByTransferVolumeDataRole fixes WHICH roles under
// templates/rbacv2 may be drawn into that aggregate. The axis is just a label, and it selects with
// rbac.deckhouse.io/kind: use — which the neighbouring view and edit roles carry as well. So a copy of
// the label on one of them would fold its rules into the very role an external backup agent holds,
// wherever that aggregate is bound: for the edit role that means mutating verbs on the transfer
// objects, on the capture and restore requests and on VolumeSnapshots. Nothing else here would notice
// it — TestNoAggregatedRoleGrantsVolumeDataTransfer inspects the "<resource>/download" pairs, which
// those roles do not grant at all.
//
// Limit: the walk covers templates/rbacv2 and nothing beyond it, so "no other role" here means no
// other role under THAT directory, not of this chart. Every other ClusterRole this chart defines sits
// in a templated file — the rbac-for-us.yaml ones and templates/user-authz-cluster-roles.yaml — which
// the flat YAML parser of loadRBACv2ClusterRoles cannot read, so widening the walk is not a matter of
// changing a path: a check covering them has to render the chart. Several of those roles hold mutating
// verbs on the transfer objects, and the exempt axis appearing on one of them would go unnoticed here
// and by the ban below alike, since the ban looks at the "<resource>/download" pairs and they grant
// none. Beyond the chart there is nothing this file can do at all: the aggregate collects any
// ClusterRole in the cluster carrying the labels it selects on, a cluster administrator's own
// included, and no chart pins that set.
func TestBackupAgentAggregationIsCarriedOnlyByTransferVolumeDataRole(t *testing.T) {
	roles, files, documents := loadRBACv2ClusterRoles(t)
	t.Logf("read %d YAML file(s) and %d document(s) under %s, %d ClusterRole(s)",
		files, documents, rbacV2Dir, len(roles))
	require.NotEmpty(t, roles, "no ClusterRole was read, so this check compared nothing")

	var carriers []string
	for _, role := range roles {
		if _, ok := aggregationLabels(role)[backupAgentAggregationLabel]; ok {
			carriers = append(carriers, role.Name)
		}
	}
	t.Logf("inspected %d ClusterRole(s) under %s, %d of them carry %s",
		len(roles), rbacV2Dir, len(carriers), backupAgentAggregationLabel)

	assert.ElementsMatch(t, []string{transferVolumeDataRoleName}, carriers,
		"%s may be carried by %q and by no other role under %s: the label alone is enough to fold a role "+
			"into the aggregate an external backup agent is bound to",
		backupAgentAggregationLabel, transferVolumeDataRoleName, rbacV2Dir)
}

// TestNoAggregatedRoleGrantsVolumeDataTransfer keeps the permission behind an explicit binding.
// Moving a volume out of the cluster and back in is granted to a named subject on purpose; an
// aggregation label on any role carrying these subresources would hand the same reach to every
// subject of a cluster-wide access level, silently and without a binding anyone can point at.
//
// Exactly one axis is exempt, backupAgentAggregationLabel, and the exemption is the same rule applied
// to a different mechanism rather than a hole in it: the axes banned here are held by everyone at the
// corresponding access level from the moment the label appears, while the role collecting the exempt
// axis is itself granted to a named subject by an explicit RoleBinding and reaches nobody until then.
// The exemption is per LABEL, not per role — a role declaring the exempt axis alongside any other one
// is still inspected and still fails. The room it does leave is covered by two checks above, one for
// each half: TestTransferVolumeDataRoleAggregatesOnlyIntoBackupAgent pins WHICH axes
// transferVolumeDataRoleName may declare, its value included, and
// TestBackupAgentAggregationIsCarriedOnlyByTransferVolumeDataRole pins WHICH roles may declare the
// exempt axis at all. The second one is what keeps the exemption from being as wide as it looks:
// without it any role under templates/rbacv2 could take the exemption and nothing here would object.
//
// Limit: it inspects the roles under templates/rbacv2 only. A role defined elsewhere in the chart, or
// written by a cluster administrator, is outside its reach.
func TestNoAggregatedRoleGrantsVolumeDataTransfer(t *testing.T) {
	roles, files, documents := loadRBACv2ClusterRoles(t)
	t.Logf("read %d YAML file(s) and %d document(s) under %s, %d ClusterRole(s)",
		files, documents, rbacV2Dir, len(roles))

	pairs := authorizedSubresources()
	require.NotEmpty(t, pairs, "no transfer direction is authorized, so this check compared nothing")

	aggregated, exempt := 0, 0
	for _, role := range roles {
		var levelAxes []string
		hasExemptAxis := false
		for label := range aggregationLabels(role) {
			if label == backupAgentAggregationLabel {
				hasExemptAxis = true

				continue
			}
			levelAxes = append(levelAxes, label)
		}
		if len(levelAxes) == 0 {
			if hasExemptAxis {
				exempt++
			}

			continue
		}
		aggregated++
		slices.Sort(levelAxes)

		granted := grantedSubresources(t, role)
		for _, pair := range pairs {
			assert.NotContainsf(t, granted, pair,
				"%q carries %s, so its rules reach every subject of that access level, and it must not grant %q on %s",
				role.Name, strings.Join(levelAxes, ", "), AuthzVerb, pair)
		}
	}
	t.Logf("inspected %d aggregated ClusterRole(s) out of %d; %d exempt on %s alone",
		aggregated, len(roles), exempt, backupAgentAggregationLabel)
	require.NotZero(t, aggregated, "no aggregated ClusterRole was inspected, so this check compared nothing")
}
