{{/* Expand the chart name. */}}
{{- define "kama.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a stable, DNS-safe release name. */}}
{{- define "kama.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/* Namespace-qualified name for resources whose scope is the whole cluster. */}}
{{- define "kama.clusterScopedName" -}}
{{- printf "%s-%s" (include "kama.fullname" .) .Release.Namespace | trunc 220 | trimSuffix "-" -}}
{{- end }}

{{/* Chart label value. */}}
{{- define "kama.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Common identifying labels. */}}
{{- define "kama.labels" -}}
helm.sh/chart: {{ include "kama.chart" . }}
{{ include "kama.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: kama
{{- end }}

{{/* Immutable selector labels. */}}
{{- define "kama.selectorLabels" -}}
app.kubernetes.io/name: {{ include "kama.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: manager
{{- end }}

{{/* Service account name. */}}
{{- define "kama.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "kama.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- required "serviceAccount.name is required when serviceAccount.create=false" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/* Image reference; digest wins over tag. */}}
{{- define "kama.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end }}
