{{- define "kwerft.fullname" -}}kwerft{{- end }}

{{- define "kwerft.labels" -}}
app.kubernetes.io/name: kwerft
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{- define "kwerft.selectorLabels" -}}
app.kubernetes.io/name: kwerft
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kwerft.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}

{{- /* The registry (zot), internal/builds.RegistryService. */ -}}
{{- define "kwerft.registry.selectorLabels" -}}
app.kubernetes.io/name: kwerft-registry
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "kwerft.registry.labels" -}}
{{ include "kwerft.registry.selectorLabels" . }}
app.kubernetes.io/component: registry
app.kubernetes.io/part-of: kwerft
app.kubernetes.io/version: {{ .Values.registry.image.tag | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}
