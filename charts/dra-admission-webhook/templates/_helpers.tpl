{{/*
Chart name, truncated to 63 chars.
*/}}
{{- define "dra-admission-webhook.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name, truncated to 63 chars.
*/}}
{{- define "dra-admission-webhook.fullname" -}}
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
Chart label value.
*/}}
{{- define "dra-admission-webhook.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "dra-admission-webhook.labels" -}}
helm.sh/chart: {{ include "dra-admission-webhook.chart" . }}
{{ include "dra-admission-webhook.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels (used in matchLabels).
*/}}
{{- define "dra-admission-webhook.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dra-admission-webhook.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Webhook component labels.
*/}}
{{- define "dra-admission-webhook.webhookLabels" -}}
{{ include "dra-admission-webhook.labels" . }}
app.kubernetes.io/component: webhook
{{- end }}

{{/*
Webhook selector labels.
*/}}
{{- define "dra-admission-webhook.webhookSelectorLabels" -}}
{{ include "dra-admission-webhook.selectorLabels" . }}
app.kubernetes.io/component: webhook
{{- end }}

{{/*
Reconciler component labels.
*/}}
{{- define "dra-admission-webhook.reconcilerLabels" -}}
{{ include "dra-admission-webhook.labels" . }}
app.kubernetes.io/component: reconciler
{{- end }}

{{/*
Reconciler selector labels.
*/}}
{{- define "dra-admission-webhook.reconcilerSelectorLabels" -}}
{{ include "dra-admission-webhook.selectorLabels" . }}
app.kubernetes.io/component: reconciler
{{- end }}

{{/*
Namespace to deploy into.
*/}}
{{- define "dra-admission-webhook.namespace" -}}
{{- default .Release.Namespace .Values.namespace }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "dra-admission-webhook.serviceAccountName" -}}
{{- include "dra-admission-webhook.fullname" . }}
{{- end }}

{{/*
ConfigMap name.
*/}}
{{- define "dra-admission-webhook.configmapName" -}}
{{- printf "%s-config" (include "dra-admission-webhook.fullname" .) }}
{{- end }}

{{/*
TLS secret name.
*/}}
{{- define "dra-admission-webhook.tlsSecretName" -}}
{{- .Values.tls.secretName }}
{{- end }}

{{/*
Service name.
*/}}
{{- define "dra-admission-webhook.serviceName" -}}
{{- include "dra-admission-webhook.fullname" . }}
{{- end }}

{{/*
Webhook image.
*/}}
{{- define "dra-admission-webhook.webhookImage" -}}
{{- printf "%s:%s" .Values.webhook.image.repository (default .Chart.AppVersion .Values.webhook.image.tag) }}
{{- end }}

{{/*
Reconciler image.
*/}}
{{- define "dra-admission-webhook.reconcilerImage" -}}
{{- printf "%s:%s" .Values.reconciler.image.repository (default .Chart.AppVersion .Values.reconciler.image.tag) }}
{{- end }}

{{/*
Generate TLS certificate data. Returns a dict with keys: crt, key, ca.
Uses lookup to preserve existing certs across upgrades.
*/}}
{{- define "dra-admission-webhook.tlsCerts" -}}
{{- $ns := include "dra-admission-webhook.namespace" . -}}
{{- $secretName := include "dra-admission-webhook.tlsSecretName" . -}}
{{- $svcName := include "dra-admission-webhook.serviceName" . -}}
{{- $cn := printf "%s.%s.svc" $svcName $ns -}}
{{- $altNames := list $cn (printf "%s.%s.svc.cluster.local" $svcName $ns) -}}
{{- $existing := lookup "v1" "Secret" $ns $secretName -}}
{{- if $existing -}}
  {{- $caCert := index $existing.data "ca.crt" | b64dec -}}
  {{- $tlsCert := index $existing.data "tls.crt" | b64dec -}}
  {{- $tlsKey := index $existing.data "tls.key" | b64dec -}}
  {{- dict "ca" $caCert "crt" $tlsCert "key" $tlsKey | toJson -}}
{{- else -}}
  {{- $ca := genCA "dra-webhook-ca" 365 -}}
  {{- $cert := genSignedCert $cn nil $altNames 365 $ca -}}
  {{- dict "ca" $ca.Cert "crt" $cert.Cert "key" $cert.Key | toJson -}}
{{- end -}}
{{- end }}
