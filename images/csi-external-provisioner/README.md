# csi-external-provisioner

Builds a patched `kubernetes-csi/external-provisioner` (`werf.inc.yaml`, versions resolved from
`oss.yaml`), published as `csi-external-provisioner-ForK8SGE-<k8s>`. See `patches/<version>/README.md`
for what each patch does; `patches/v6.3.0/README.md` also documents the VolumeRestoreRequest executor
this build carries.

## Bump checklist

When bumping the `csi-external-provisioner` version pinned in `oss.yaml`:

1. Re-apply the existing patches (or re-derive them) against the new tag and confirm the build is green,
   same as any other patched image in this repository.
2. **Re-read `pkg/controller/controller.go`'s `prepareProvision`/`provision` switch on
   `spec.dataSource`/`spec.dataSourceRef.Kind`.** `storage-foundation`'s `DataImport` CreatePVC mode parks
   a target PVC whose `dataSourceRef` names an unrecognized kind (`DataImport`) by relying on this
   provisioner treating that as `&controller.IgnoredError{...}` ("assuming an external populator will
   provision it") instead of provisioning an empty volume under it. Nothing in this repository's patches
   touches that branch, and no feature gate is involved — but nothing stops a future upstream release from
   changing it either.
3. The `git clone`/`git apply` steps in `werf.inc.yaml`'s `src-artifact` stage already fail the build with
   `grep -q 'assuming an external populator will provision it' pkg/controller/controller.go` if that
   branch disappears from the cloned tree, both before and after this repository's own patches are
   applied. A build failure there means step 2 needs a real answer, not a version bump of the grep.
4. This guard only covers the copy built here. Other Deckhouse modules run this same
   `external-provisioner` code as a sidecar inside their own CSI driver's controller Pod (their own
   `oss.yaml` pin, their own bump cadence) — those copies are outside this repository's reach, and their
   own bump checklists need the same re-read.
