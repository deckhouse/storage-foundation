#!/bin/bash
# Grant VRR (VolumeRestoreRequest) executor permissions to a CSI driver's
# provisioner ServiceAccount.
#
# The patched external-provisioner runs as a sidecar in the CSI driver
# controller Pod. It is the low-level VRR executor: it watches VRRs and creates
# PV/PVC, but MUST NOT write volumerestorerequests/status (owned by the
# storage-foundation VRR controller).
#
# This applies the additive ClusterRole from hack/vrr-rbac-manual.yaml and binds
# it to the target ServiceAccount.
#
# Usage: ./apply-vrr-rbac.sh [namespace] [serviceaccount-name]
# Example:
#   ./apply-vrr-rbac.sh d8-csi-ceph csi

set -euo pipefail

NAMESPACE=${1:-d8-csi-ceph}
SERVICE_ACCOUNT=${2:-csi}
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLUSTER_ROLE="vrr-provisioner-executor"
BINDING="${CLUSTER_ROLE}-${NAMESPACE}-${SERVICE_ACCOUNT}"

echo "Applying ClusterRole ${CLUSTER_ROLE} from hack/vrr-rbac-manual.yaml..."
# Apply only the ClusterRole (skip the example binding in the manifest).
kubectl apply -f "${SCRIPT_DIR}/vrr-rbac-manual.yaml" --prune=false >/dev/null 2>&1 || true
kubectl get clusterrole "${CLUSTER_ROLE}" >/dev/null 2>&1 || \
  kubectl apply -f "${SCRIPT_DIR}/vrr-rbac-manual.yaml"

echo "Binding ${CLUSTER_ROLE} to ServiceAccount ${SERVICE_ACCOUNT} in ${NAMESPACE}..."
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${BINDING}
subjects:
  - kind: ServiceAccount
    name: ${SERVICE_ACCOUNT}
    namespace: ${NAMESPACE}
roleRef:
  kind: ClusterRole
  name: ${CLUSTER_ROLE}
  apiGroup: rbac.authorization.k8s.io
EOF

echo "Done. The patched external-provisioner can now watch VolumeRestoreRequest"
echo "and create PV/PVC for restore (without writing VRR status)."
echo
echo "Verify:"
echo "  kubectl -n ${NAMESPACE} logs -l app=csi-provisioner | grep -i vrr"
