{{- define "storage-foundation.grants_crd_has" -}}
  {{- $ctx := index . 0 -}}
  {{- $fields := ((($ctx.Values.storageFoundation).internal).grantsCRDFields) | default dict -}}
  {{- index $fields (index . 1) | default false -}}
{{- end -}}
