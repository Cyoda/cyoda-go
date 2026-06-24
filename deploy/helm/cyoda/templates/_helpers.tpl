{{/*
Expand the name of the chart.
*/}}
{{- define "cyoda.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "cyoda.fullname" -}}
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

{{/*
Chart-name label (for chart-version tracking).
*/}}
{{- define "cyoda.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels applied to every rendered resource.
*/}}
{{- define "cyoda.labels" -}}
helm.sh/chart: {{ include "cyoda.chart" . }}
{{ include "cyoda.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels (stable across upgrades).
*/}}
{{- define "cyoda.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cyoda.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
ServiceAccount name.
*/}}
{{- define "cyoda.serviceAccountName" -}}
{{- default (include "cyoda.fullname" .) .Values.serviceAccount.name }}
{{- end }}

{{/*
Chart-managed HMAC Secret name (used when no existingSecret is provided).
*/}}
{{- define "cyoda.hmacSecretName" -}}
{{- if .Values.cluster.hmacSecret.existingSecret -}}
{{ .Values.cluster.hmacSecret.existingSecret }}
{{- else -}}
{{ printf "%s-hmac" (include "cyoda.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Chart-managed bootstrap client Secret name.
*/}}
{{- define "cyoda.bootstrapSecretName" -}}
{{- if .Values.bootstrap.clientSecret.existingSecret -}}
{{ .Values.bootstrap.clientSecret.existingSecret }}
{{- else -}}
{{ printf "%s-bootstrap" (include "cyoda.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Chart-managed metrics bearer-token Secret name.
*/}}
{{- define "cyoda.metricsBearerSecretName" -}}
{{- if .Values.monitoring.metricsBearer.existingSecret -}}
{{ .Values.monitoring.metricsBearer.existingSecret }}
{{- else -}}
{{ printf "%s-metrics-bearer" (include "cyoda.fullname" .) }}
{{- end }}
{{- end }}

{{/*
Image reference: falls back to .Chart.AppVersion if image.tag is unset.
*/}}
{{- define "cyoda.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s:%s" .Values.image.repository $tag -}}
{{- end }}

{{/*
Migration-Job DSN secret (name + key). When migrate.postgres.existingSecret is
set, the Job uses that secret and its key; otherwise BOTH fall back to the
runtime postgres secret (single-DSN, backward-compatible). Name and key fall
back together as a unit, so a custom postgres.existingSecretKey is honored by
the migration Job in single-DSN mode.
*/}}
{{- define "cyoda.migrateSecretName" -}}
{{- if .Values.migrate.postgres.existingSecret -}}
{{- .Values.migrate.postgres.existingSecret -}}
{{- else -}}
{{- .Values.postgres.existingSecret -}}
{{- end -}}
{{- end -}}

{{- define "cyoda.migrateSecretKey" -}}
{{- if .Values.migrate.postgres.existingSecret -}}
{{- .Values.migrate.postgres.existingSecretKey -}}
{{- else -}}
{{- .Values.postgres.existingSecretKey -}}
{{- end -}}
{{- end -}}
