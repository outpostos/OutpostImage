{{/*
Peers headless Service name.
Uses discovery.kubernetes.serviceName if set, otherwise "<release>-peers".
*/}}
{{- define "outpostimage.peersServiceName" -}}
{{- if .Values.discovery.kubernetes.serviceName -}}
{{ .Values.discovery.kubernetes.serviceName }}
{{- else -}}
{{ .Release.Name }}-peers
{{- end -}}
{{- end -}}

{{/*
ServiceAccount name.
*/}}
{{- define "outpostimage.serviceAccountName" -}}
{{- if .Values.serviceAccount.name -}}
{{ .Values.serviceAccount.name }}
{{- else -}}
{{ .Release.Name }}
{{- end -}}
{{- end -}}
