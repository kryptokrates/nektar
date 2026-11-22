{{- define "nektar-agent.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nektar-agent.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "nektar-agent.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
selectorLabels stay minimal ON PURPOSE: StatefulSet selectors are
immutable — anything added here (version, managed-by) breaks helm
upgrade. Version and friends belong in .labels, which is used for
resource metadata, never for selectors.
*/}}
{{- define "nektar-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "nektar-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "nektar-agent.labels" -}}
{{- include "nektar-agent.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}

{{- define "nektar-agent.secretName" -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else }}
{{- include "nektar-agent.fullname" . }}
{{- end }}
{{- end }}
