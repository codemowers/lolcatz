{{/* Prefix short image names; allow fully qualified image overrides. */}}
{{- define "lolcatz.image" -}}
{{- if and .registry (not (contains "/" .image)) -}}
{{- printf "%s/%s" (trimSuffix "/" .registry) .image -}}
{{- else -}}
{{- .image -}}
{{- end -}}
{{- end -}}

{{/* Keep application listener names and internal URLs consistent. */}}
{{- define "lolcatz.internalScheme" -}}
{{- ternary "https" "http" .Values.internalTLS.enabled -}}
{{- end -}}

{{/* Components without an enabled value are always deployed. Component names
are kebab-case; their values keys are camelCase. */}}
{{- define "lolcatz.componentEnabled" -}}
{{- $key := .name | replace "-" "_" | camelcase | untitle -}}
{{- dig "enabled" true (index .values $key | default dict) -}}
{{- end -}}
