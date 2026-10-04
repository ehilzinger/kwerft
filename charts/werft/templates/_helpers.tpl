{{- define "werft.fullname" -}}werft{{- end }}

{{- define "werft.labels" -}}
app.kubernetes.io/name: werft
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{- define "werft.selectorLabels" -}}
app.kubernetes.io/name: werft
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "werft.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end }}
