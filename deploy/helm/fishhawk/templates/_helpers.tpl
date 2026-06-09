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
Common labels. Accepts either the chart root context (`.`) — the back-compat
call shape, byte-identical to the pre-split output — or a dict
`(dict "root" $ "role" R)` so split-mode Deployments stamp an
`app.kubernetes.io/component` label (threaded through fishhawk.selectorLabels).
*/}}
{{- define "fishhawk.labels" -}}
{{- $root := . -}}
{{- if hasKey . "root" -}}{{- $root = .root -}}{{- end -}}
helm.sh/chart: {{ include "fishhawk.chart" $root }}
{{ include "fishhawk.selectorLabels" . }}
{{- if $root.Chart.AppVersion }}
app.kubernetes.io/version: {{ $root.Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ $root.Release.Service }}
{{- end -}}

{{/*
Selector labels. Accepts either the chart root context (`.`) — emitting the bare
two-label set (byte-identical to the allInOne / pre-split call sites) — or a dict
`(dict "root" $ "role" R)` where a non-empty role (api|worker) adds an
`app.kubernetes.io/component: <role>` label. The component label lets the split-
mode Service select only the api pods, excluding the worker pod from its
endpoints.
*/}}
{{- define "fishhawk.selectorLabels" -}}
{{- $root := . -}}
{{- $role := "" -}}
{{- if hasKey . "root" -}}{{- $root = .root -}}{{- $role = .role -}}{{- end -}}
app.kubernetes.io/name: {{ include "fishhawk.name" $root }}
app.kubernetes.io/instance: {{ $root.Release.Name }}
{{- if $role }}
app.kubernetes.io/component: {{ $role }}
{{- end }}
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
{{- if .Values.jaeger.enabled -}}
{{- fail "jaeger.enabled deploys an ephemeral, unauthenticated all-in-one trace collector and is DEV/DOGFOODING-ONLY: set profile=local to acknowledge, or disable jaeger for a real deploy." -}}
{{- end -}}
{{- end -}}
{{- if and (eq .Values.secrets.mode "externalSecrets") (not .Values.secrets.externalSecrets.secretStoreRef.name) -}}
{{- fail "secrets.mode=externalSecrets requires secrets.externalSecrets.secretStoreRef.name to be set (the SecretStore/ClusterSecretStore the ExternalSecret reads from)." -}}
{{- end -}}
{{- end -}}

{{/*
Topology guard (#851). `include`d once from deployment.yaml (the allInOne
Deployment), so it runs on every allInOne render. Calls `fail` when
deployment.mode=allInOne is combined with replicaCount>1 AND any worker toggle is
on — that would run duplicate background-worker singletons (SLA timer, dispatch
watchdog, reaction poller, merge reconciler, child-completion sweeper, invariant
monitor), racing the timers/reconcilers. The message names an offending toggle
and the two safe ways out (split mode, or replicaCount=1). NOTE: this guard has
no split-mode include site — deployment.yaml does not render in split mode, so a
future split-mode render-time guard must be wired into deployment-worker.yaml /
deployment-api.yaml instead (see the comment in deployment-worker.yaml).
*/}}
{{- define "fishhawk.validateTopology" -}}
{{- if eq .Values.deployment.mode "allInOne" -}}
{{- if gt (int .Values.replicaCount) 1 -}}
{{- $offending := "" -}}
{{- range $k, $v := .Values.workers -}}
{{- if and $v (not $offending) -}}{{- $offending = printf "workers.%s" $k -}}{{- end -}}
{{- end -}}
{{- if $offending -}}
{{- fail (printf "deployment.mode=allInOne with replicaCount=%d and an enabled worker toggle (%s) would run duplicate background-worker singletons and race the timers/reconcilers. Either set deployment.mode=split to scale the API tier (workers stay on a single -worker Deployment), or keep replicaCount=1." (int .Values.replicaCount) $offending) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Topology-mode guard (#910). `include`d once from service.yaml — the only template
that renders in EVERY topology mode (allInOne and split both emit a Service; its
document is not wrapped in a top-level mode `if`). Calls `fail` when
deployment.mode is neither "allInOne" nor "split": such a value makes all three
Deployment templates skip their `if eq` guards, silently rendering a chart with a
Service + ConfigMap but zero Deployments — a confusing no-op install. The
deployment*.yaml templates can't host this guard because they themselves don't
render on an unrecognized mode. The message names the bad value and the two valid
choices.
*/}}
{{- define "fishhawk.validateMode" -}}
{{- if not (or (eq .Values.deployment.mode "allInOne") (eq .Values.deployment.mode "split")) -}}
{{- fail (printf "deployment.mode=%q is not recognized: set it to \"allInOne\" (default) or \"split\"." .Values.deployment.mode) -}}
{{- end -}}
{{- end -}}

{{/*
fishhawkd pod spec — the single source of truth for the pod template shared by
the allInOne Deployment (role "all"), the split-mode `-api` Deployment (role
"api"), and the split-mode `-worker` Deployment (role "worker"). Invoke with a
dict `(dict "root" $ "role" R)`; the chart root is threaded explicitly via the
`root` key rather than relying on `.`. The only role-dependent output is the
FISHHAWKD_ENABLE_* worker env: role "api" forces every toggle to "false" (the api
tier runs no background workers); roles "all" and "worker" honor .Values.workers.*.
*/}}
{{- define "fishhawk.fishhawkdPodSpec" -}}
{{- $root := .root -}}
{{- $role := .role -}}
containers:
  - name: fishhawkd
    image: "{{ $root.Values.image.repository }}:{{ $root.Values.image.tag | default $root.Chart.AppVersion }}"
    imagePullPolicy: {{ $root.Values.image.pullPolicy }}
    ports:
      - name: http
        containerPort: 8080
        protocol: TCP
    envFrom:
      # Non-secret FISHHAWKD_* env.
      - configMapRef:
          name: {{ include "fishhawk.fullname" $root }}-config
      # Sensitive FISHHAWKD_* env (DB URL, API keys, OAuth secret, webhook
      # secret, AWS creds). Name resolved by fishhawk.secretName across all
      # three secrets modes (#849); optional:false means the pod fails loud
      # if it's absent. The GitHub App private key lives in the SAME Secret
      # under a dotted key, which envFrom skips — it is mounted as a file
      # (see volumes/volumeMounts below), never injected as env.
      - secretRef:
          name: {{ include "fishhawk.secretName" $root }}
    env:
      {{- if $root.Values.postgres.enabled }}
      # In-cluster Postgres (postgres.enabled). A container-level env entry
      # overrides the same key arriving via the existingSecret envFrom, so
      # this URL is authoritative for the local stack without editing the
      # secret. sslmode=disable matches the plaintext in-cluster Service.
      - name: FISHHAWKD_DATABASE_URL
        value: "postgres://{{ $root.Values.postgres.auth.user }}:{{ $root.Values.postgres.auth.password }}@{{ include "fishhawk.fullname" $root }}-postgres:{{ $root.Values.postgres.service.port }}/{{ $root.Values.postgres.auth.database }}?sslmode=disable"
      {{- end }}
      # Background-worker enable toggles → FISHHAWKD_ENABLE_*. Role "api" forces
      # every toggle off (workers run only on the allInOne pod or the -worker
      # Deployment); roles "all" and "worker" honor .Values.workers.*.
      - name: FISHHAWKD_ENABLE_SLA_TIMER
        value: {{ if eq $role "api" }}"false"{{ else }}{{ $root.Values.workers.slaTimer | quote }}{{ end }}
      - name: FISHHAWKD_ENABLE_DISPATCH_WATCHDOG
        value: {{ if eq $role "api" }}"false"{{ else }}{{ $root.Values.workers.dispatchWatchdog | quote }}{{ end }}
      - name: FISHHAWKD_ENABLE_REACTION_POLLER
        value: {{ if eq $role "api" }}"false"{{ else }}{{ $root.Values.workers.reactionPoller | quote }}{{ end }}
      - name: FISHHAWKD_ENABLE_MERGE_RECONCILER
        value: {{ if eq $role "api" }}"false"{{ else }}{{ $root.Values.workers.mergeReconciler | quote }}{{ end }}
      - name: FISHHAWKD_ENABLE_CHILD_COMPLETION_SWEEPER
        value: {{ if eq $role "api" }}"false"{{ else }}{{ $root.Values.workers.childCompletionSweeper | quote }}{{ end }}
      - name: FISHHAWKD_ENABLE_INVARIANT_MONITOR
        value: {{ if eq $role "api" }}"false"{{ else }}{{ $root.Values.workers.invariantMonitor | quote }}{{ end }}
    livenessProbe:
      httpGet:
        path: {{ $root.Values.probes.liveness.path }}
        port: http
      initialDelaySeconds: {{ $root.Values.probes.liveness.initialDelaySeconds }}
      periodSeconds: {{ $root.Values.probes.liveness.periodSeconds }}
      timeoutSeconds: {{ $root.Values.probes.liveness.timeoutSeconds }}
      failureThreshold: {{ $root.Values.probes.liveness.failureThreshold }}
    readinessProbe:
      httpGet:
        path: {{ $root.Values.probes.readiness.path }}
        port: http
      initialDelaySeconds: {{ $root.Values.probes.readiness.initialDelaySeconds }}
      periodSeconds: {{ $root.Values.probes.readiness.periodSeconds }}
      timeoutSeconds: {{ $root.Values.probes.readiness.timeoutSeconds }}
      failureThreshold: {{ $root.Values.probes.readiness.failureThreshold }}
    resources:
      {{- toYaml $root.Values.resources | nindent 6 }}
    {{- if $root.Values.secrets.githubApp.privateKeyFile.enabled }}
    volumeMounts:
      # GitHub App private key, projected read-only as a single file from
      # the Secret (fishhawk.secretName). subPath mounts just the file, so
      # the rest of the directory is untouched.
      - name: github-app-private-key
        mountPath: {{ $root.Values.secrets.githubApp.privateKeyFile.mountPath | quote }}
        subPath: {{ $root.Values.secrets.githubApp.privateKeyFile.mountPath | base | quote }}
        readOnly: true
    {{- end }}
{{- if $root.Values.secrets.githubApp.privateKeyFile.enabled }}
volumes:
  - name: github-app-private-key
    secret:
      secretName: {{ include "fishhawk.secretName" $root }}
      items:
        # Project ONLY the PEM key to the mount path's basename. The dotted
        # secretKey is skipped by envFrom but surfaced here as a file.
        - key: {{ $root.Values.secrets.githubApp.privateKeyFile.secretKey | quote }}
          path: {{ $root.Values.secrets.githubApp.privateKeyFile.mountPath | base | quote }}
{{- end }}
{{- end -}}
