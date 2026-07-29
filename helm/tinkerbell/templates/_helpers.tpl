{{/*
Ensure a value is a JSON array. Fails on nil or non-array input.
- slice/array: use as-is
- nil/missing: fail with an error (required field)
- anything else: fail with a type error
Usage: {{ include "tinkerbell.toJsonArray" .apiGroups }}
*/}}
{{- define "tinkerbell.toJsonArray" -}}
{{- if kindIs "slice" . -}}
  {{- . | toJson -}}
{{- else if kindIs "invalid" . -}}
  {{- fail "required field is nil/missing in rbac.additionalRoleRules entry (apiGroups, resources, and verbs are required and must be arrays)" -}}
{{- else -}}
  {{- fail (printf "expected an array but got %s: %v" (kindOf .) .) -}}
{{- end -}}
{{- end -}}

{{/*
Render an optional RBAC field as a JSON array. Skips nil/missing values.
- slice/array: render as JSON array
- nil/missing: output nothing (caller uses 'with' or checks result)
- anything else: fail with a type error
Usage: {{ include "tinkerbell.toOptionalJsonArray" .resourceNames }}
*/}}
{{- define "tinkerbell.toOptionalJsonArray" -}}
{{- if kindIs "slice" . -}}
  {{- . | toJson -}}
{{- else if not (kindIs "invalid" .) -}}
  {{- fail (printf "expected an array but got %s: %v" (kindOf .) .) -}}
{{- end -}}
{{- end -}}
{{/* vim: set filetype=mustache: */}}
{{/*
Expand the name of the chart.
*/}}
{{- define "tinkerbell.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "tinkerbell.fullname" -}}
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
Create chart name and version as used by the chart label.
*/}}
{{- define "tinkerbell.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create the name of the service account
*/}}
{{- define "tinkerbell.serviceAccountName" -}}
{{- printf "%s-service-account" .Values.rbac.name }}
{{- end }}

{{/*
Allow the release namespace to be overridden for multi-namespace deployments in combined charts
*/}}
{{- define "tinkerbell.namespace" -}}
{{- if .Values.namespaceOverride }}
{{- .Values.namespaceOverride }}
{{- else }}
{{- .Release.Namespace }}
{{- end }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "tinkerbell.labels" -}}
helm.sh/chart: {{ include "tinkerbell.chart" . }}
{{ include "tinkerbell.selectorLabels" . }}
{{- if or .Chart.AppVersion .Values.deployment.imageTag }}
app.kubernetes.io/version: {{ coalesce .Values.deployment.imageTag .Chart.AppVersion | trunc 63 | trimSuffix "-" | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with .Values.extraLabels }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "tinkerbell.selectorLabels" -}}
app.kubernetes.io/name: {{ include "tinkerbell.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Generate environment variables from a values structure.
Usage: {{ include "tinkerbell.generateEnvVars" (dict "values" .Values.deployment.envs.globals) }}
*/}}
{{- define "tinkerbell.generateEnvVars" -}}
{{- range $sKey, $sValue := . }}
{{- range $key, $value := $sValue }}
{{- if $value }}
- name: {{ list "TINKERBELL" (ternary "" (snakecase $sKey | upper) (eq (upper $sKey) "GLOBALS")) (snakecase $key | upper) | compact | join "_" }}
  value: {{ quote $value }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Generate all Tinkerbell environment variables from deployment.envs structure.
This generates env vars for all sections: globals, smee, tinkServer, tinkController, rufio, secondstar, tootles
*/}}
{{- define "tinkerbell.allEnvVars" -}}
{{- $root := . }}
{{- $publicIP := .Values.publicIP }}
{{- $trustedProxies := .Values.trustedProxies }}
{{- if .Values.deployment.hostNetwork }}
- name: HOST_IP
  valueFrom:
    fieldRef:
      fieldPath: status.hostIP
{{- end }}
{{- include "tinkerbell.generateEnvVars" .Values.deployment.envs }}
- name: TINKERBELL_TRUSTED_PROXIES
  value: {{ join "," $trustedProxies | quote }}
- name: TINKERBELL_PUBLIC_IPV4
  value: {{ coalesce .Values.deployment.envs.globals.PUBLIC_IPV4 $publicIP | quote }}
{{- with .Values.deployment.additionalEnvs }}
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end }}

{{/*
Service account name for leader election RBAC
*/}}
{{- define "tinkerbell.leaderElection.serviceAccountName" -}}
{{- default (printf "%s-service-account" (include "tinkerbell.fullname" .)) .Values.rbac.name }}
{{- end }}

{{/*
Generate container ports based on enabled services
*/}}
{{- define "tinkerbell.containerPorts" -}}
{{- if .Values.deployment.envs.globals.ENABLE_SMEE }}
- containerPort: {{ .Values.deployment.envs.smee.TFTP_SERVER_BIND_PORT }}
{{- with .Values.service.ports.tftp }}
  name: {{ .name }}
  protocol: {{ .protocol }}
{{- end }}
- containerPort: {{ .Values.deployment.envs.smee.SYSLOG_BIND_PORT }}
{{- with .Values.service.ports.syslog }}
  name: {{ .name }}
  protocol: {{ .protocol }}
{{- end }}
{{- with .Values.service.ports.dhcp }}
- containerPort: {{ .port }}
  name: {{ .name }}
  protocol: {{ .protocol }}
{{- end }}
- containerPort: {{ .Values.deployment.envs.smee.IPXE_HTTP_SCRIPT_BIND_PORT }}
{{- with .Values.service.ports.httpSmee }}
  name: {{ .name }}
  protocol: {{ .protocol }}
{{- end }}
{{- end }}
{{- if .Values.deployment.envs.globals.ENABLE_TOOTLES }}
- containerPort: {{ .Values.deployment.envs.tootles.TOOTLES_BIND_PORT }}
{{- with .Values.service.ports.httpTootles }}
  name: {{ .name }}
  protocol: {{ .protocol }}
{{- end }}
{{- end }}
{{- if .Values.deployment.envs.globals.ENABLE_TINK_SERVER }}
- containerPort: {{ .Values.deployment.envs.tinkServer.TINK_SERVER_BIND_PORT }}
{{- with .Values.service.ports.grpc }}
  name: {{ .name }}
  protocol: {{ .protocol }}
{{- end }}
{{- end }}
{{- if .Values.deployment.envs.globals.ENABLE_SECONDSTAR }}
- containerPort: {{ .Values.deployment.envs.secondstar.SECONDSTAR_PORT }}
{{- with .Values.service.ports.ssh }}
  name: {{ .name }}
  protocol: {{ .protocol }}
{{- end }}
{{- end }}
{{- if and .Values.deployment.envs.globals.TLS_CERT_FILE .Values.deployment.envs.globals.TLS_KEY_FILE }}
- containerPort: {{ .Values.deployment.envs.smee.HTTPS_BIND_PORT }}
{{- with .Values.service.ports.https }}
  name: {{ .name }}
  protocol: {{ .protocol }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Image pull secrets handling
*/}}
{{- define "tinkerbell.imagePullSecrets" -}}
{{- if .Values.imagePullSecrets }}
imagePullSecrets:
{{- range .Values.imagePullSecrets }}
{{- if typeIs "string" . }}
- name: {{ . }}
{{- else }}
- name: {{ .name }}
{{- end }}
{{- end }}
{{- else if .Values.global.imagePullSecrets }}
imagePullSecrets:
{{- range .Values.global.imagePullSecrets }}
{{- if typeIs "string" . }}
- name: {{ . }}
{{- else }}
- name: {{ .name }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
