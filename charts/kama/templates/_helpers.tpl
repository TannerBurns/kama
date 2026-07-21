{{/* Expand the chart name. */}}
{{- define "kama.name" -}}
{{- default .Chart.Name .Values.nameOverride | replace "." "-" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/* Create a stable, DNS-safe release name. */}}
{{- define "kama.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | replace "." "-" | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | replace "." "-" | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | replace "." "-" | trunc 63 | trimSuffix "-" }}
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

{{/* Importer image reference; digest wins over tag. */}}
{{- define "kama.importerImage" -}}
{{- if .Values.importer.image.digest -}}
{{- printf "%s@%s" .Values.importer.image.repository .Values.importer.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.importer.image.repository (default .Chart.AppVersion .Values.importer.image.tag) -}}
{{- end -}}
{{- end }}

{{/* Comma-separated importer image pull Secret names for the manager flag. */}}
{{- define "kama.importerImagePullSecrets" -}}
{{- $names := list -}}
{{- range .Values.importer.imagePullSecrets -}}
{{- if kindIs "string" . -}}
{{- $names = append $names . -}}
{{- else -}}
{{- $names = append $names .name -}}
{{- end -}}
{{- end -}}
{{- join "," $names -}}
{{- end }}

{{/* CPU runtime image reference; digest wins over tag. */}}
{{- define "kama.runtimeCPUImage" -}}
{{- if .Values.runtime.cpu.image.digest -}}
{{- printf "%s@%s" .Values.runtime.cpu.image.repository .Values.runtime.cpu.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.runtime.cpu.image.repository (default .Chart.AppVersion .Values.runtime.cpu.image.tag) -}}
{{- end -}}
{{- end }}

{{/* CUDA runtime image reference; digest wins over tag. */}}
{{- define "kama.runtimeCUDAImage" -}}
{{- if .Values.runtime.cuda.image.digest -}}
{{- printf "%s@%s" .Values.runtime.cuda.image.repository .Values.runtime.cuda.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.runtime.cuda.image.repository (default .Chart.AppVersion .Values.runtime.cuda.image.tag) -}}
{{- end -}}
{{- end }}

{{/* Comma-separated runtime image pull Secret names for the manager flag. */}}
{{- define "kama.runtimeImagePullSecrets" -}}
{{- $names := list -}}
{{- range .Values.runtime.imagePullSecrets -}}
{{- if kindIs "string" . -}}
{{- $names = append $names . -}}
{{- else -}}
{{- $names = append $names .name -}}
{{- end -}}
{{- end -}}
{{- join "," $names -}}
{{- end }}

{{/* Helm-owned webhook TLS Secret. */}}
{{- define "kama.webhookTLSSecretName" -}}
{{- if .Values.webhook.tls.secretName -}}
{{- .Values.webhook.tls.secretName -}}
{{- else -}}
{{- $base := include "kama.fullname" . | trunc 51 | trimSuffix "-" -}}
{{- printf "%s-webhook-tls" $base -}}
{{- end -}}
{{- end }}

{{/* Dedicated ClusterIP admission Service name. */}}
{{- define "kama.webhookServiceName" -}}
{{- $base := include "kama.fullname" . | trunc 55 | trimSuffix "-" -}}
{{- printf "%s-webhook" $base -}}
{{- end }}
