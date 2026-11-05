{{- define "llmgate.name" -}}{{ .Release.Name }}{{- end -}}

{{- define "llmgate.labels" -}}
app.kubernetes.io/name: llmgate
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "llmgate.image" -}}{{ .Values.image.repository }}:{{ .Values.image.tag }}{{- end -}}

{{/* Restricted pod security: non-root, read-only root, no privileges. */}}
{{- define "llmgate.securityContext" -}}
allowPrivilegeEscalation: false
readOnlyRootFilesystem: true
runAsNonRoot: true
capabilities:
  drop: ["ALL"]
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{- define "llmgate.secretEnv" -}}
{{- range $name, $key := .Values.secretEnv }}
- name: {{ $name }}
  valueFrom:
    secretKeyRef:
      name: {{ $.Values.secret.name }}
      key: {{ $key }}
{{- end }}
{{- end -}}
