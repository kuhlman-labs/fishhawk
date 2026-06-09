{{/*
Expand the name of the chart.
*/}}
{{- define "fishhawk.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create a fully qualified app name. Truncated at 63 chars for k8s name limits.
*/}}
{{- define "fishhawk.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Chart name and version, as used by the chart label.
*/}}
{{- define "fishhawk.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels.
*/}}
{{- define "fishhawk.labels" -}}
helm.sh/chart: {{ include "fishhawk.chart" . }}
{{ include "fishhawk.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels.
*/}}
{{- define "fishhawk.selectorLabels" -}}
app.kubernetes.io/name: {{ include "fishhawk.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Secret name — the single source of truth the Deployment + migrate Job reference
across all three secrets modes (no template duplication). `existing` reads the
operator-supplied existingSecret; `chartManaged` and `externalSecrets` both use
the chart-owned `<fullname>-secrets` name (chartManaged renders that Secret;
externalSecrets has ESO materialize a Secret of the same name via its target).
*/}}
{{- define "fishhawk.secretName" -}}
{{- if eq .Values.secrets.mode "existing" -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- printf "%s-secrets" (include "fishhawk.fullname" .) -}}
{{- end -}}
{{- end -}}

{{/*
External URL consumed by the ConfigMap (FISHHAWKD_EXTERNAL_URL). Explicit
config.externalUrl always wins. Otherwise, when the ingress is enabled with a
host, derive `https://<host>` (`http://<host>` when ingress.tls is off). When
neither an explicit value nor an enabled ingress host exists, return empty so the
key stays unset (ignore-if-unset semantics).
*/}}
{{- define "fishhawk.externalUrl" -}}
{{- if .Values.config.externalUrl -}}
{{- .Values.config.externalUrl -}}
{{- else if and .Values.ingress.enabled .Values.ingress.host -}}
{{- $scheme := ternary "https" "http" .Values.ingress.tls.enabled -}}
{{- printf "%s://%s" $scheme .Values.ingress.host -}}
{{- end -}}
{{- end -}}

{{/*
OAuth callback URL consumed by the ConfigMap (FISHHAWKD_OAUTH_CALLBACK_URL).
Explicit config.oauthCallbackUrl always wins. Otherwise, when the ingress is
enabled with a host, derive `<scheme>://<host>/v0/auth/github/callback` — the
path fishhawkd registers the GitHub OAuth callback handler at (serve.go). Empty
when neither an explicit value nor an enabled ingress host exists.
*/}}
{{- define "fishhawk.oauthCallbackUrl" -}}
{{- if .Values.config.oauthCallbackUrl -}}
{{- .Values.config.oauthCallbackUrl -}}
{{- else if and .Values.ingress.enabled .Values.ingress.host -}}
{{- $scheme := ternary "https" "http" .Values.ingress.tls.enabled -}}
{{- printf "%s://%s/v0/auth/github/callback" $scheme .Values.ingress.host -}}
{{- end -}}
{{- end -}}

{{/*
Deploy-time guard (#847 carry-over). `include`d once from the Deployment so every
render runs it. Calls `fail` when a dev-only convenience is active outside the
`local` profile:
  - secrets.mode == chartManaged (the chart would bake plaintext secrets);
  - in-cluster Postgres with the well-known default password `fishhawk`;
  - in-cluster MinIO with the well-known default rootPassword `fishhawk-dev-secret`.
Independently, `externalSecrets` mode requires a non-empty secretStoreRef.name in
any profile. The message names the offending toggle and the override required.
*/}}
{{- define "fishhawk.validateSecrets" -}}
{{- if ne .Values.profile "local" -}}
{{- if eq .Values.secrets.mode "chartManaged" -}}
{{- fail "secrets.mode=chartManaged renders plaintext secrets into the chart and is DEV-ONLY: set profile=local to acknowledge, or switch to secrets.mode=existing/externalSecrets for prod." -}}
{{- end -}}
{{- if and .Values.postgres.enabled (eq .Values.postgres.auth.password "fishhawk") -}}
{{- fail "postgres.enabled with the default password 'fishhawk' is DEV-ONLY: set profile=local to acknowledge, or override postgres.auth.password for a real deploy." -}}
{{- end -}}
{{- if and .Values.minio.enabled (eq .Values.minio.rootPassword "fishhawk-dev-secret") -}}
{{- fail "minio.enabled with the default rootPassword 'fishhawk-dev-secret' is DEV-ONLY: set profile=local to acknowledge, or override minio.rootPassword for a real deploy." -}}
{{- end -}}
{{- end -}}
{{- if and (eq .Values.secrets.mode "externalSecrets") (not .Values.secrets.externalSecrets.secretStoreRef.name) -}}
{{- fail "secrets.mode=externalSecrets requires secrets.externalSecrets.secretStoreRef.name to be set (the SecretStore/ClusterSecretStore the ExternalSecret reads from)." -}}
{{- end -}}
{{- end -}}
