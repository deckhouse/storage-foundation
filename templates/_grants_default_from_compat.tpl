{{- define "storage-foundation.grants_default_from_additional_keys" -}}
  {{- $raw := (.Values.global).deckhouseVersion | default "dev" | toString -}}
  {{- $mm := regexFind "^v?[0-9]+[.][0-9]+" $raw -}}
  {{- if $mm -}}
    {{- semverCompare ">= 1.79" (printf "%s.0" $mm) -}}
  {{- else -}}
    true
  {{- end -}}
{{- end -}}
