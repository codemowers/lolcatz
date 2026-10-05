{{/* Prefix short image names; allow fully qualified image overrides. */}}
{{- define "lolcatz.image" -}}
{{- if and .registry (not (contains "/" .image)) -}}
{{- printf "%s/%s" (trimSuffix "/" .registry) .image -}}
{{- else -}}
{{- .image -}}
{{- end -}}
{{- end -}}

{{/* Pod label granting egress to the OIDC issuer in egress-isolated namespaces. */}}
{{- define "lolcatz.oidcIssuerLabels" -}}
{{- if .Values.oidcIssuerAccess.ingressAddress }}
codemowers.io/oidc-issuer-access: "true"
{{- end }}
{{- end -}}

{{/* Resolve the OIDC issuer to the address its egress allowance names. */}}
{{- define "lolcatz.oidcIssuerHostAliases" -}}
{{- with .Values.oidcIssuerAccess }}
{{- if .ingressAddress }}
hostAliases:
  - ip: {{ .ingressAddress | quote }}
    hostnames:
      - {{ required "oidcIssuerAccess.host is required with oidcIssuerAccess.ingressAddress" .host | quote }}
{{- end }}
{{- end }}
{{- end -}}

