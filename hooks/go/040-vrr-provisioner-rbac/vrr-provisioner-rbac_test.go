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

package hooks_common

import (
	"reflect"
	"testing"

	"github.com/deckhouse/storage-foundation/hooks/go/consts"
)

func TestBindingName(t *testing.T) {
	got := bindingName("d8-sds-local-volume")
	want := "d8:storage-foundation:vrr-provisioner-executor:d8-sds-local-volume"
	if got != want {
		t.Fatalf("bindingName = %q, want %q", got, want)
	}
	// Must be derived from the ClusterRole name so the two never drift.
	if got != consts.VRRProvisionerExecutorClusterRoleName+":d8-sds-local-volume" {
		t.Fatalf("bindingName not derived from ClusterRole name: %q", got)
	}
}

func TestDesiredClusterRole(t *testing.T) {
	cr := desiredClusterRole()

	if cr.Name != consts.VRRProvisionerExecutorClusterRoleName {
		t.Errorf("ClusterRole name = %q, want %q", cr.Name, consts.VRRProvisionerExecutorClusterRoleName)
	}
	if !reflect.DeepEqual(cr.Labels, map[string]string{"heritage": "deckhouse", "module": "storage-foundation"}) {
		t.Errorf("ClusterRole labels = %v", cr.Labels)
	}
	if len(cr.Rules) != 2 {
		t.Fatalf("expected exactly 2 rules, got %d: %+v", len(cr.Rules), cr.Rules)
	}

	vrrRule := cr.Rules[0]
	if !reflect.DeepEqual(vrrRule.APIGroups, []string{"storage.deckhouse.io"}) {
		t.Errorf("vrr apiGroups = %v", vrrRule.APIGroups)
	}
	if !reflect.DeepEqual(vrrRule.Resources, []string{"volumerestorerequests"}) {
		t.Errorf("vrr resources = %v", vrrRule.Resources)
	}
	if !reflect.DeepEqual(vrrRule.Verbs, []string{"get", "list", "watch"}) {
		t.Errorf("vrr verbs = %v, want read-only get/list/watch (executor must not write VRR/status)", vrrRule.Verbs)
	}

	pvcRule := cr.Rules[1]
	if !reflect.DeepEqual(pvcRule.APIGroups, []string{""}) {
		t.Errorf("pvc apiGroups = %v, want core group", pvcRule.APIGroups)
	}
	if !reflect.DeepEqual(pvcRule.Resources, []string{"persistentvolumeclaims"}) {
		t.Errorf("pvc resources = %v", pvcRule.Resources)
	}
	// create is the verb whose absence stalls the restore (driver roles grant only
	// get/list/watch/update on PVCs); assert it is present.
	hasCreate := false
	for _, v := range pvcRule.Verbs {
		if v == "create" {
			hasCreate = true
			break
		}
	}
	if !hasCreate {
		t.Errorf("pvc verbs = %v, want to include create (executor creates the target PVC)", pvcRule.Verbs)
	}
}

func TestDesiredClusterRoleBinding(t *testing.T) {
	const ns = "d8-sds-local-volume"
	crb := desiredClusterRoleBinding(ns)

	if crb.Name != bindingName(ns) {
		t.Errorf("binding name = %q, want %q", crb.Name, bindingName(ns))
	}
	if !reflect.DeepEqual(crb.Labels, map[string]string{"heritage": "deckhouse", "module": "storage-foundation"}) {
		t.Errorf("binding labels = %v", crb.Labels)
	}
	if crb.RoleRef.Kind != "ClusterRole" ||
		crb.RoleRef.Name != consts.VRRProvisionerExecutorClusterRoleName ||
		crb.RoleRef.APIGroup != "rbac.authorization.k8s.io" {
		t.Errorf("roleRef = %+v", crb.RoleRef)
	}
	if len(crb.Subjects) != 1 {
		t.Fatalf("expected exactly 1 subject, got %d: %+v", len(crb.Subjects), crb.Subjects)
	}
	sub := crb.Subjects[0]
	if sub.Kind != "ServiceAccount" || sub.Name != consts.CSIServiceAccountName || sub.Namespace != ns {
		t.Errorf("subject = %+v, want ServiceAccount %q in %q", sub, consts.CSIServiceAccountName, ns)
	}
}
