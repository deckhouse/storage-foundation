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
//
// The chart ships every role twice, behind a gate on global.deckhouseVersion: the capability/scope
// model of DKP 1.78 and later, and the legacy manage/use model below it. Both are what some cluster
// runs, so every check below is made against each of the two schemes on its own.
const (
	// repoRoot is this repository's root as seen from images/data-exporter/internal/repository.
	repoRoot = "../../../.."

	// rbacV2Dir holds the roles a cluster administrator grants to people and service accounts.
	rbacV2Dir = "templates/rbacv2"

	// The exact lines the RBACv2 migration script writes around the two objects of a template, and
	// the one-sided gate of a file that ships the legacy object only. Nothing else is recognized:
	// see splitSchemeBranches.
	newSchemeGate    = `{{- if eq (include "storage-foundation.rbacv2_new_scheme" .) "true" }}`
	legacySchemeGate = `{{- if ne (include "storage-foundation.rbacv2_new_scheme" .) "true" }}`
	schemeElse       = `{{- else }}`
	schemeEnd        = `{{- end }}`

	// transferVolumeDataRoleName and legacyTransferVolumeDataRoleName are spelled out here on purpose.
	// They are public names that documentation and RoleBindings outside this repository refer to, the
	// first on DKP 1.78 and later and the second below it, so they are part of what the module
	// promises and cannot be changed to suit a refactoring.
	transferVolumeDataRoleName       = "d8:namespace-capability:storage-foundation:transfer_volume_data"
	legacyTransferVolumeDataRoleName = "d8:use:capability:module:storage-foundation:transfer_volume_data"

	// aggregationLabelPrefix marks a role whose rules are folded into an access level of the platform,
	// reaching every subject of that level without anyone binding the role itself.
	aggregationLabelPrefix = "rbac.deckhouse.io/aggregate-to-"

	// backupAgentAggregationLabel is what the new scheme uses in place of the legacy backup-agent axis.
	// It lies outside rbac.deckhouse.io/, so no platform role selects on it: it is collected by a single
	// ClusterRole, d8:state-snapshotter:backup-agent, which grants nothing until somebody binds it. A
	// role carrying it still reaches exactly the subjects of an explicit binding, which is the property
	// TestNoAggregatedRoleGrantsVolumeDataTransfer exists to protect, so it is exempt from that ban.
	backupAgentAggregationLabel = "state-snapshotter.deckhouse.io/aggregate-to-backup-agent"

	// backupAgentAggregationValue is the value transferVolumeDataRoleName must carry on that label. It
	// has to equal the value the aggregating role selects on, and nothing in this repository can check
	// that: a mismatch is silent, the role simply stays out of the aggregated set, and the transfer
	// then answers 403 to a subject whose binding looks correct.
	backupAgentAggregationValue = "true"

	// legacyBackupAgentAggregationLabel is the one axis under aggregationLabelPrefix that does NOT put
	// the rules of a role behind a cluster-wide access level, and therefore the legacy scheme's
	// exemption from TestNoAggregatedRoleGrantsVolumeDataTransfer. It is collected by a single
	// ClusterRole, d8:use:capability:module:state-snapshotter:backup_agent, which grants nothing until
	// somebody binds it. Every other axis of the legacy scheme stays banned.
	legacyBackupAgentAggregationLabel = aggregationLabelPrefix + "backup-agent-as"

	// legacyBackupAgentAggregationValue is the value legacyTransferVolumeDataRoleName must carry on
	// that axis, under the same silent-mismatch caveat as backupAgentAggregationValue.
	legacyBackupAgentAggregationValue = "member"

	// namespaceAggregationLabel and namespaceAdminLevel are the one platform level the new transfer
	// capability aggregates into. The 1.78 contract refuses a capability that aggregates into no
	// platform role, so the backup-agent label cannot be the only one; the namespace lineage is
	// granted per namespace through a RoleBinding, and its admin level can already reach the contents
	// of a volume in its namespace (pods/exec into the pod mounting it), so the transfer adds nothing
	// new in kind there. The exemption is for this role at this level only: any other role, or this
	// role at any other level or lineage, stays banned.
	namespaceAggregationLabel = aggregationLabelPrefix + "namespace-as"
	namespaceAdminLevel       = "admin"

	// i18n annotations every capability of the new model carries, and no legacy object does.
	enTitleAnnotation       = "en.meta.deckhouse.io/title"
	ruTitleAnnotation       = "ru.meta.deckhouse.io/title"
	enDescriptionAnnotation = "en.meta.deckhouse.io/description"
	ruDescriptionAnnotation = "ru.meta.deckhouse.io/description"
)

// rbacScheme is one of the two role models the chart renders, with the facts the checks below pin
// for it.
type rbacScheme struct {
	name string

	// newModel selects the branch of a gated template this scheme renders.
	newModel bool

	// roleNames is every ClusterRole the scheme ships under templates/rbacv2, compared exactly.
	roleNames []string

	// transferRole is the name of the volume data transfer role in this scheme.
	transferRole string

	// backupAgentLabel and backupAgentValue are the label, and its value, by which the backup agent
	// role of this scheme collects the transfer role.
	backupAgentLabel string
	backupAgentValue string

	// foreignBackupAgentLabel is the backup-agent label of the OTHER scheme, which no role of this one
	// may carry: the aggregate of this scheme does not select on it, and the aggregate of the other one
	// is not installed alongside.
	foreignBackupAgentLabel string

	// transferAxes is the exact set of aggregation labels, with values, the transfer role carries.
	transferAxes map[string]string

	// transferExemptAxes are the level axes, with values, the transfer role may carry despite
	// granting the transfer subresources. Nothing else is exempt, on this role or on any other.
	transferExemptAxes map[string]string
}

var (
	newRBACScheme = rbacScheme{
		name:     "DKP 1.78 and later",
		newModel: true,
		roleNames: []string{
			"d8:system-capability:storage-foundation:view",
			"d8:system-capability:storage-foundation:edit",
			"d8:namespace-capability:storage-foundation:view",
			"d8:namespace-capability:storage-foundation:edit",
			transferVolumeDataRoleName,
		},
		transferRole:            transferVolumeDataRoleName,
		backupAgentLabel:        backupAgentAggregationLabel,
		backupAgentValue:        backupAgentAggregationValue,
		foreignBackupAgentLabel: legacyBackupAgentAggregationLabel,
		transferAxes: map[string]string{
			namespaceAggregationLabel:   namespaceAdminLevel,
			backupAgentAggregationLabel: backupAgentAggregationValue,
		},
		transferExemptAxes: map[string]string{namespaceAggregationLabel: namespaceAdminLevel},
	}

	legacyRBACScheme = rbacScheme{
		name:     "DKP below 1.78",
		newModel: false,
		roleNames: []string{
			"d8:manage:permission:module:storage-foundation:view",
			"d8:manage:permission:module:storage-foundation:edit",
			"d8:use:capability:module:storage-foundation:view",
			"d8:use:capability:module:storage-foundation:edit",
			legacyTransferVolumeDataRoleName,
		},
		transferRole:            legacyTransferVolumeDataRoleName,
		backupAgentLabel:        legacyBackupAgentAggregationLabel,
		backupAgentValue:        legacyBackupAgentAggregationValue,
		foreignBackupAgentLabel: backupAgentAggregationLabel,
		transferAxes:            map[string]string{legacyBackupAgentAggregationLabel: legacyBackupAgentAggregationValue},
		transferExemptAxes:      map[string]string{},
	}

	rbacSchemes = []rbacScheme{newRBACScheme, legacyRBACScheme}
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

// splitSchemeBranches returns the part of a template the new scheme renders and the part the legacy
// scheme renders. It understands exactly the shapes the chart uses and nothing more:
//
//   - no Helm action at all: both schemes render the whole file;
//   - newSchemeGate on the first line, schemeElse, schemeEnd on the last non-empty line: the new
//     scheme renders what is between the gate and the else, the legacy one what is between the else
//     and the end;
//   - newSchemeGate or legacySchemeGate on the first line and schemeEnd on the last non-empty line,
//     with no else: that one scheme renders the body, the other one renders nothing.
//
// Any other "{{" in the file — another action, a gate in a different spelling, a gate that does not
// span the whole file, a gate inside a comment — fails the test. Guessing at a template it does not
// understand would let a guard below read the wrong branch, or none, and still pass; a file like that
// needs the chart rendered, and this helper to be replaced by a renderer rather than stretched.
func splitSchemeBranches(t *testing.T, path string, raw []byte) (newPart, legacyPart []byte) {
	t.Helper()

	lines := strings.Split(string(raw), "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}

	var actions []int
	for i, line := range lines {
		if strings.Contains(line, "{{") {
			actions = append(actions, i)
		}
	}
	if len(actions) == 0 {
		return raw, raw
	}

	join := func(from, to int) []byte { return []byte(strings.Join(lines[from:to], "\n") + "\n") }
	lineIs := func(i int, want string) bool { return strings.TrimRight(lines[i], " \t\r") == want }

	switch {
	case len(actions) == 3 && actions[0] == 0 && actions[2] == last &&
		lineIs(0, newSchemeGate) && lineIs(actions[1], schemeElse) && lineIs(last, schemeEnd):
		return join(1, actions[1]), join(actions[1]+1, last)
	case len(actions) == 2 && actions[0] == 0 && actions[1] == last &&
		lineIs(0, newSchemeGate) && lineIs(last, schemeEnd):
		return join(1, last), nil
	case len(actions) == 2 && actions[0] == 0 && actions[1] == last &&
		lineIs(0, legacySchemeGate) && lineIs(last, schemeEnd):
		return nil, join(1, last)
	}

	require.FailNowf(t, "unsupported template",
		"%s carries Helm actions on line(s) %v that are not the scheme gate this test knows how to read; "+
			"the guards here must render the chart to cover it", path, actions)

	return nil, nil
}

// loadRBACv2ClusterRoles parses every ClusterRole the given scheme ships under templates/rbacv2 and
// reports how much was read, so that "nothing violates the rule" can be told apart from "nothing was
// examined".
//
// Limit: the files are read as plain YAML once splitSchemeBranches has picked the branch of the
// scheme. That is sound only because this part of the chart carries no Helm actions other than that
// gate today (unlike, say, templates/user-authz-cluster-roles.yaml, which does). Should a role here
// gain other templating, splitSchemeBranches fails on it, and this helper must learn to render the
// chart — narrowing its scope to the files that still parse would quietly shrink the guard to nothing.
func loadRBACv2ClusterRoles(t *testing.T, scheme rbacScheme) (roles []rbacv1.ClusterRole, files, documents int) {
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
		newPart, legacyPart := splitSchemeBranches(t, path, raw)
		part := legacyPart
		if scheme.newModel {
			part = newPart
		}

		reader := yaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(part)))
		for {
			document, err := reader.Read()
			if errors.Is(err, io.EOF) {
				return nil
			}
			require.NoErrorf(t, err, "splitting %s (%s) into YAML documents", path, scheme.name)
			if len(bytes.TrimSpace(document)) == 0 {
				continue
			}
			documents++

			var role rbacv1.ClusterRole
			require.NoErrorf(t, yaml.Unmarshal(document, &role), "parsing %s (%s) as a ClusterRole", path, scheme.name)
			if role.Kind != "ClusterRole" {
				continue
			}
			roles = append(roles, role)
		}
	})
	require.NoErrorf(t, err, "walking %s", dir)

	return roles, files, documents
}

// loadSchemeRoles is loadRBACv2ClusterRoles with the bookkeeping every check repeats: it logs what was
// read and fails when nothing was.
func loadSchemeRoles(t *testing.T, scheme rbacScheme) []rbacv1.ClusterRole {
	t.Helper()

	roles, files, documents := loadRBACv2ClusterRoles(t, scheme)
	t.Logf("%s: read %d YAML file(s) and %d document(s) under %s, %d ClusterRole(s)",
		scheme.name, files, documents, rbacV2Dir, len(roles))
	require.NotEmptyf(t, roles, "%s: no ClusterRole was read, so this check compared nothing", scheme.name)

	return roles
}

// findClusterRole returns the role with the given name, failing the test when it is absent.
func findClusterRole(t *testing.T, roles []rbacv1.ClusterRole, scheme rbacScheme, name string) rbacv1.ClusterRole {
	t.Helper()

	for _, role := range roles {
		if role.Name == name {
			return role
		}
	}
	require.FailNowf(t, "ClusterRole not found", "%q is not defined under %s for %s", name, rbacV2Dir, scheme.name)

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

// TestRBACv2TemplatesShipBothSchemes pins which roles each scheme ships, by name, and the shape that
// tells the two models apart: every object of the new one is a capability with a scope and the four
// i18n annotations the console shows, and no legacy object carries any of that. It is also what makes
// the other checks meaningful for both schemes — a gate that renders the same branch for both, or an
// object that ended up on the wrong side of it, fails here first.
func TestRBACv2TemplatesShipBothSchemes(t *testing.T) {
	for _, scheme := range rbacSchemes {
		t.Run(scheme.name, func(t *testing.T) {
			roles := loadSchemeRoles(t, scheme)

			names := make([]string, 0, len(roles))
			for _, role := range roles {
				names = append(names, role.Name)

				kind := role.Labels["rbac.deckhouse.io/kind"]
				_, hasScope := role.Labels["rbac.deckhouse.io/scope"]
				if scheme.newModel {
					assert.Equalf(t, "capability", kind, "%q must be a capability of the new model", role.Name)
					assert.Truef(t, hasScope, "%q must carry rbac.deckhouse.io/scope", role.Name)
					for _, annotation := range []string{
						enTitleAnnotation, ruTitleAnnotation, enDescriptionAnnotation, ruDescriptionAnnotation,
					} {
						assert.NotEmptyf(t, role.Annotations[annotation], "%q must carry %s", role.Name, annotation)
					}

					continue
				}
				assert.Containsf(t, []string{"use", "manage"}, kind, "%q must keep its legacy kind", role.Name)
				assert.Falsef(t, hasScope, "%q is a legacy object and must not carry rbac.deckhouse.io/scope", role.Name)
				assert.Emptyf(t, role.Annotations, "%q is a legacy object and must render as it did before", role.Name)
			}

			assert.ElementsMatchf(t, scheme.roleNames, names,
				"%s must ship exactly these roles under %s", scheme.name, rbacV2Dir)
		})
	}
}

// TestTransferVolumeDataRoleGrantsAuthorizedSubresources is the link between the authorization code
// and the shipped role: the role must grant AuthzVerb on exactly the pairs AuthorizeUser asks the API
// server about — no fewer (the client gets 403 on a direction the role forgot) and no more (the role
// hands out a permission nothing checks). Held for the role of each scheme.
func TestTransferVolumeDataRoleGrantsAuthorizedSubresources(t *testing.T) {
	want := authorizedSubresources()
	require.NotEmpty(t, want, "no transfer direction is authorized, so this check compared nothing")

	for _, scheme := range rbacSchemes {
		t.Run(scheme.name, func(t *testing.T) {
			role := findClusterRole(t, loadSchemeRoles(t, scheme), scheme, scheme.transferRole)

			assert.ElementsMatch(t, want, grantedSubresources(t, role),
				"%q must grant %q on exactly the subresources AuthorizeUser authorizes against",
				scheme.transferRole, AuthzVerb)
		})
	}
}

// aggregationLabels returns the labels by which the role is collected into another role, keyed by
// label: every "rbac.deckhouse.io/aggregate-to-*" label, plus the scheme's backup-agent label when it
// lies outside that prefix. A check can then talk about the axes a role declares without restating
// the other labels.
func aggregationLabels(role rbacv1.ClusterRole, scheme rbacScheme) map[string]string {
	axes := make(map[string]string)
	for label, value := range role.Labels {
		if strings.HasPrefix(label, aggregationLabelPrefix) || label == scheme.backupAgentLabel {
			axes[label] = value
		}
	}

	return axes
}

// assertTransferRoleAxes compares the aggregation labels of the scheme's transfer role with
// scheme.transferAxes, exactly.
func assertTransferRoleAxes(t *testing.T, scheme rbacScheme) {
	t.Helper()

	role := findClusterRole(t, loadSchemeRoles(t, scheme), scheme, scheme.transferRole)

	axes := aggregationLabels(role, scheme)
	t.Logf("%q declares %d aggregation label(s) among its %d label(s)", role.Name, len(axes), len(role.Labels))

	assert.Equal(t, scheme.transferAxes, axes,
		"%q must declare exactly these aggregation labels: their values are what put the role into the "+
			"aggregated sets it belongs to, and any other axis would fold the transfer permission into an "+
			"access level that is not meant to hold it", role.Name)
}

// TestTransferVolumeDataRoleAggregatesOnlyIntoBackupAgent pins which aggregation axes the legacy role
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
	assertTransferRoleAxes(t, legacyRBACScheme)
}

// TestTransferVolumeDataCapabilityAggregatesOnlyIntoNamespaceAdminAndBackupAgent is the same pin for
// the capability of the new scheme, which aggregates in two places: into the namespace lineage at the
// admin level, and through the module label into d8:state-snapshotter:backup-agent. Both values are
// compared, and so is the absence of anything else: no other level of the namespace lineage (viewer,
// user and manager are held by subjects who are not meant to move volume data), no subsystem or
// system lineage (those are granted cluster-wide), and not the legacy backup-agent axis, which the
// 1.78 contract refuses as an unknown lineage.
//
// Limit: as above, only these labels are examined. An aggregationRule selecting on the kind, scope or
// module label alone would collect this capability too, and nothing in this chart can see that.
func TestTransferVolumeDataCapabilityAggregatesOnlyIntoNamespaceAdminAndBackupAgent(t *testing.T) {
	assertTransferRoleAxes(t, newRBACScheme)
}

// TestBackupAgentAggregationIsCarriedOnlyByTransferVolumeDataRole fixes WHICH roles under
// templates/rbacv2 may be drawn into the backup-agent aggregate, in each scheme. The label is just a
// label, and the aggregate selects on it together with the kind — rbac.deckhouse.io/kind: use in the
// legacy scheme, rbac.deckhouse.io/kind: capability in the new one — which the neighbouring view and
// edit roles carry as well. So a copy of the label on one of them would fold its rules into the very
// role an external backup agent holds, wherever that aggregate is bound: for the edit role that means
// mutating verbs on the transfer objects, on the capture and restore requests and on VolumeSnapshots.
// Nothing else here would notice it — TestNoAggregatedRoleGrantsVolumeDataTransfer inspects the
// "<resource>/download" pairs, which those roles do not grant at all. The backup-agent label of the
// other scheme is banned on every role outright: its aggregate is not installed alongside, so on the
// transfer role it would mean the label the installed aggregate selects on was swapped for a dead one.
//
// Limit: the walk covers templates/rbacv2 and nothing beyond it, so "no other role" here means no
// other role under THAT directory, not of this chart. Every other ClusterRole this chart defines sits
// in a templated file — the rbac-for-us.yaml ones and templates/user-authz-cluster-roles.yaml — which
// splitSchemeBranches refuses and the flat YAML parser of loadRBACv2ClusterRoles cannot read, so
// widening the walk is not a matter of changing a path: a check covering them has to render the chart.
// Several of those roles hold mutating verbs on the transfer objects, and the label appearing on one
// of them would go unnoticed here and by the ban below alike, since the ban looks at the
// "<resource>/download" pairs and they grant none. Beyond the chart there is nothing this file can do
// at all: the aggregate collects any ClusterRole in the cluster carrying the labels it selects on, a
// cluster administrator's own included, and no chart pins that set.
func TestBackupAgentAggregationIsCarriedOnlyByTransferVolumeDataRole(t *testing.T) {
	for _, scheme := range rbacSchemes {
		t.Run(scheme.name, func(t *testing.T) {
			roles := loadSchemeRoles(t, scheme)

			var carriers []string
			for _, role := range roles {
				if _, ok := role.Labels[scheme.backupAgentLabel]; ok {
					carriers = append(carriers, role.Name)
				}
				assert.NotContainsf(t, role.Labels, scheme.foreignBackupAgentLabel,
					"%q carries %s, which the backup-agent aggregate of %s does not select on",
					role.Name, scheme.foreignBackupAgentLabel, scheme.name)
			}
			t.Logf("inspected %d ClusterRole(s) under %s, %d of them carry %s",
				len(roles), rbacV2Dir, len(carriers), scheme.backupAgentLabel)

			assert.ElementsMatch(t, []string{scheme.transferRole}, carriers,
				"%s may be carried by %q and by no other role under %s: the label alone is enough to fold a "+
					"role into the aggregate an external backup agent is bound to",
				scheme.backupAgentLabel, scheme.transferRole, rbacV2Dir)
		})
	}
}

// TestNoAggregatedRoleGrantsVolumeDataTransfer keeps the permission behind an explicit binding, or a
// level that already reaches a volume's contents. Moving a volume out of the cluster and back in is
// granted to a named subject on purpose; an aggregation label on any role carrying these
// subresources would hand the same reach to every subject of an access level, silently and without a
// binding anyone can point at.
//
// The backup-agent label of each scheme is exempt, and the exemption is the same rule applied to a
// different mechanism rather than a hole in it: the axes banned here are held by everyone at the
// corresponding access level from the moment the label appears, while the role collecting the
// backup-agent label is itself granted to a named subject by an explicit binding and reaches nobody
// until then. That exemption is per LABEL, not per role — a role declaring it alongside any other
// axis is still inspected and still fails.
//
// The new scheme adds one more exemption, and it is narrower: per ROLE, LABEL and VALUE. The transfer
// capability may aggregate into the namespace lineage at the admin level (see
// namespaceAggregationLabel for why that level and no other), and that is all — the same label with
// another value, or on any other role, is inspected like any other axis.
//
// The room the exemptions leave is covered by the checks above:
// TestTransferVolumeDataRoleAggregatesOnlyIntoBackupAgent and
// TestTransferVolumeDataCapabilityAggregatesOnlyIntoNamespaceAdminAndBackupAgent pin WHICH axes the
// transfer role may declare, values included, and
// TestBackupAgentAggregationIsCarriedOnlyByTransferVolumeDataRole pins WHICH roles may declare the
// backup-agent label at all. The last one is what keeps that exemption from being as wide as it
// looks: without it any role under templates/rbacv2 could take it and nothing here would object.
//
// Limit: it inspects the roles under templates/rbacv2 only. A role defined elsewhere in the chart, or
// written by a cluster administrator, is outside its reach.
func TestNoAggregatedRoleGrantsVolumeDataTransfer(t *testing.T) {
	pairs := authorizedSubresources()
	require.NotEmpty(t, pairs, "no transfer direction is authorized, so this check compared nothing")

	for _, scheme := range rbacSchemes {
		t.Run(scheme.name, func(t *testing.T) {
			roles := loadSchemeRoles(t, scheme)

			aggregated, exempt := 0, 0
			for _, role := range roles {
				var levelAxes []string
				hasExemptAxis := false
				for label, value := range aggregationLabels(role, scheme) {
					if label == scheme.backupAgentLabel {
						hasExemptAxis = true

						continue
					}
					if want, ok := scheme.transferExemptAxes[label]; ok && role.Name == scheme.transferRole && value == want {
						hasExemptAxis = true

						continue
					}
					levelAxes = append(levelAxes, label+"="+value)
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
			t.Logf("%s: inspected %d aggregated ClusterRole(s) out of %d; %d exempt on their exempt axes alone",
				scheme.name, aggregated, len(roles), exempt)
			require.NotZerof(t, aggregated, "%s: no aggregated ClusterRole was inspected, so this check compared nothing",
				scheme.name)
			require.NotZerof(t, exempt, "%s: the transfer role was not seen on its exempt axes, so the exemption "+
				"was never exercised", scheme.name)
		})
	}
}
