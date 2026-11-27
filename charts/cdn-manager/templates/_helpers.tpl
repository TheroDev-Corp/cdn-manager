{{- define "cdn-manager.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "cdn-manager.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "cdn-manager.labels" -}}
{{ include "cdn-manager.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "cdn-manager.metricsName" -}}
{{- printf "%s-metrics" (include "cdn-manager.fullname" . | trunc 55 | trimSuffix "-") -}}
{{- end -}}

{{- define "cdn-manager.valkeyName" -}}
{{ include "cdn-manager.fullname" . | trunc 55 | trimSuffix "-" }}-valkey
{{- end -}}

{{- define "cdn-manager.redisPrefix" -}}
{{ .Values.state.redisPrefix | default (printf "cdn-manager-%s" (printf "%s/%s" .Release.Namespace .Release.Name | sha256sum | trunc 16)) }}
{{- end -}}
