{{- define "nektar-hub.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nektar-hub.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "nektar-hub.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "nektar-hub.labels" -}}
app.kubernetes.io/name: {{ include "nektar-hub.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "nektar-hub.secretName" -}}
{{- if .Values.secrets.existingSecret }}
{{- .Values.secrets.existingSecret }}
{{- else }}
{{- include "nektar-hub.fullname" . }}
{{- end }}
{{- end }}

{{/* one image tag pins every image unless a component overrides it —
    call as: include "nektar-hub.image" (dict "root" . "component" "host") */}}
{{- define "nektar-hub.image" -}}
{{- $img := index .root.Values.images .component }}
{{- $repo := $img.repository }}
{{- $digest := $img.digest | default .root.Values.images.digest }}
{{- if $digest }}{{ $repo }}@{{ $digest }}{{ else }}{{ $repo }}:{{ .root.Values.images.tag }}{{ end }}
{{- end }}
