{{- define "storage-foundation.grants_missing_path" -}}
  {{- $raw := (.Values.global).deckhouseVersion | default "dev" | toString -}}
  {{- $mm := regexFind "^v?[0-9]+[.][0-9]+" $raw -}}
  {{- if $mm -}}
    {{- semverCompare ">= 1.79" (printf "%s.0" $mm) -}}
  {{- else -}}
    true
  {{- end -}}
{{- end -}}
