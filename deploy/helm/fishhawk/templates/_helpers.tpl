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

{{/*
Credential contract — the ONE source of truth for every key the chart expects in
the Secret (E62.2 / #2301). Emits a YAML list of records, one per credential,
keyed by SECRET KEY (the key in the Secret's `data`), NOT by rendered env key:
the GitHub App PEM is not an env key at all (envFrom skips its dotted name), so
an env-keyed map had no home for it and silently omitted it from validation.

Each record is a dict serialized by `toYaml`, never hand-written YAML text, so a
dynamic, dotted or dash-bearing secretKey (the PEM's) round-trips through
`fromYamlArray` as a well-formed string instead of parsing oddly:

  secretKey    — the key in the Secret's data/stringData
  valuesField  — the field under .Values.secrets.values that supplies it in
                 chartManaged mode
  envDelivered — true when envFrom projects it as an environment variable;
                 false for the PEM, which reaches the pod only as a file
  required     — DERIVED from other values, not hardcoded: a blanket "always
                 required" would break the local profile (in-cluster Postgres
                 supplies FISHHAWKD_DATABASE_URL via a container-level env entry
                 that overrides envFrom) and any deploy with no S3 bucket or no
                 GitHub App.

Consumers: secret.yaml (renders it), fishhawk.requiredSecretKeys (filters it),
fishhawk.validateSecretContract (validates against it), NOTES.txt (prints it).
*/}}
{{- define "fishhawk.secretKeySpec" -}}
{{- $v := .Values -}}
{{- $records := list -}}
{{- $records = append $records (dict
      "secretKey" "FISHHAWKD_DATABASE_URL"
      "valuesField" "databaseUrl"
      "envDelivered" true
      "required" (not $v.postgres.enabled)) -}}
{{- $records = append $records (dict
      "secretKey" "FISHHAWKD_GITHUB_WEBHOOK_SECRET"
      "valuesField" "githubWebhookSecret"
      "envDelivered" true
      "required" true) -}}
{{- $records = append $records (dict
      "secretKey" "FISHHAWKD_OAUTH_CLIENT_SECRET"
      "valuesField" "oauthClientSecret"
      "envDelivered" true
      "required" true) -}}
{{- $records = append $records (dict
      "secretKey" "FISHHAWKD_ANTHROPIC_API_KEY"
      "valuesField" "anthropicApiKey"
      "envDelivered" true
      "required" true) -}}
{{- $records = append $records (dict
      "secretKey" "AWS_ACCESS_KEY_ID"
      "valuesField" "awsAccessKeyId"
      "envDelivered" true
      "required" (ne (toString $v.config.s3Bucket) "")) -}}
{{- $records = append $records (dict
      "secretKey" "AWS_SECRET_ACCESS_KEY"
      "valuesField" "awsSecretAccessKey"
      "envDelivered" true
      "required" (ne (toString $v.config.s3Bucket) "")) -}}
{{- $records = append $records (dict
      "secretKey" (toString $v.secrets.githubApp.privateKeyFile.secretKey)
      "valuesField" "githubAppPrivateKey"
      "envDelivered" false
      "required" $v.secrets.githubApp.privateKeyFile.enabled) -}}
{{- $records = append $records (dict
      "secretKey" "FISHHAWKD_HANDOFF_SECRET"
      "valuesField" "handoffSecret"
      "envDelivered" true
      "required" (ne (toString $v.cell.homeRegion) "")) -}}
{{- $records = append $records (dict
      "secretKey" "FISHHAWKD_MODEL_API_KEY"
      "valuesField" "modelApiKey"
      "envDelivered" true
      "required" (ne (toString $v.cell.modelBaseUrl) "")) -}}
{{- toYaml $records -}}
{{- end -}}

{{/*
The required subset of fishhawk.secretKeySpec, as a YAML list of secret keys.
Derived from the spec — never a second authored list. Consumed by
fishhawk.validateSecretContract and NOTES.txt.
*/}}
{{- define "fishhawk.requiredSecretKeys" -}}
{{- $keys := list -}}
{{- range fromYamlArray (include "fishhawk.secretKeySpec" .) -}}
{{- if .required -}}{{- $keys = append $keys .secretKey -}}{{- end -}}
{{- end -}}
{{- toYaml $keys -}}
{{- end -}}

{{/*
Credential-contract guard (E62.2 / #2301). `include`d once from service.yaml —
the only template that renders in EVERY topology mode, and which already hosts
fishhawk.validateMode for exactly that reason. deployment.yaml does not render in
split mode and secret.yaml does not render outside chartManaged, so neither can
host it.

What it can check is MODE-DEPENDENT, and the criterion says so rather than
quietly doing less:
  chartManaged    — the values ARE visible, so each required record's
                    secrets.values field must be present AND non-empty (an empty
                    string fails, naming both the secret key and the values
                    field).
  externalSecrets — the chart sees only the DECLARED data[] mapping, so each
                    required secretKey must appear in it. Whether the backing
                    store actually holds that remote ref is not observable at
                    render time (the chart does not install the ESO CRDs).
  existing        — the chart can see NOTHING about a pre-created Secret's
                    contents at render time, so the only render-time check is
                    that existingSecret is non-empty. Key presence is enforced by
                    Kubernetes at pod start (envFrom secretRef is non-optional,
                    and the PEM items: projection refuses to start the pod on a
                    missing key); VALUE-level emptiness is a live-drill item.
A `lookup`-based presence check is deliberately NOT used — see the chart README's
"Why `existing` mode has no render-time key check".
*/}}
{{- define "fishhawk.validateSecretContract" -}}
{{- $spec := fromYamlArray (include "fishhawk.secretKeySpec" .) -}}
{{- $mode := .Values.secrets.mode -}}
{{- if eq $mode "chartManaged" -}}
{{- $values := .Values.secrets.values -}}
{{- range $spec -}}
{{- if .required -}}
{{- $supplied := index $values .valuesField -}}
{{- if not $supplied -}}
{{- fail (printf "secrets.mode=chartManaged requires a non-empty secrets.values.%s (it supplies the Secret key %q, which this configuration needs). It is absent or an empty string." .valuesField .secretKey) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- else if eq $mode "externalSecrets" -}}
{{- $declared := list -}}
{{- range .Values.secrets.externalSecrets.data -}}
{{- $declared = append $declared (toString .secretKey) -}}
{{- end -}}
{{- range $spec -}}
{{- if .required -}}
{{- if not (has .secretKey $declared) -}}
{{- fail (printf "secrets.mode=externalSecrets does not map the required Secret key %q: add it to secrets.externalSecrets.data[] with its remoteRef. Declared keys: %v" .secretKey $declared) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- else if eq $mode "existing" -}}
{{- if not .Values.existingSecret -}}
{{- fail "secrets.mode=existing requires existingSecret to name the pre-created Secret carrying the sensitive FISHHAWKD_* keys; it is empty." -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{/*
Migrate-Job timing guard (E62.2 / #2301). `include`d once from migrate-job.yaml.
Inert unless migrate.activeDeadlineSeconds is SET; when it is, it recomputes
time-to-Failed from the SAME derivation values.yaml records and `fail`s when the
deadline would win the race — because a fired activeDeadlineSeconds reports
DeadlineExceeded and HIDES the migration error the failure path exists to
surface. Derived, never asserted:

  attempts   = backoffLimit + 1   (the Job controller marks Failed with reason
               BackoffLimitExceeded when status.failed EXCEEDS backoffLimit)
  backoff    = the 10s-doubling inter-attempt series (10,20,40,…, capped at
               6 minutes) summed over backoffLimit terms
  timeToFail = attempts * migrate.assumedAttemptSeconds + backoff

timeToFail is a MODEL OUTPUT and a LOWER BOUND, not an observed number: the
Kubernetes backoff series resets when no new failed pods appear, so the real
wall time can exceed it. This guard therefore only ever compares a SET deadline
AGAINST the derived figure; it never asserts the figure itself is correct. The
live drill in the chart README records the observed time-to-Failed.
*/}}
{{- define "fishhawk.validateMigrateTiming" -}}
{{- $deadline := .Values.migrate.activeDeadlineSeconds -}}
{{- if $deadline -}}
{{- $n := int .Values.migrate.backoffLimit -}}
{{- $backoff := 0 -}}
{{- $delay := 10 -}}
{{- range until $n -}}
{{- $backoff = add $backoff (min $delay 360) -}}
{{- $delay = mul $delay 2 -}}
{{- end -}}
{{- $timeToFail := add (mul (add $n 1) (int .Values.migrate.assumedAttemptSeconds)) $backoff -}}
{{- if lt (int $deadline) (int $timeToFail) -}}
{{- fail (printf "migrate.activeDeadlineSeconds=%d would fire BEFORE the Job controller can mark the migration Failed (derived time-to-Failed %ds from backoffLimit=%d and assumedAttemptSeconds=%d), so Helm would report DeadlineExceeded and hide the migration error. Raise the deadline above %ds, lower migrate.backoffLimit/assumedAttemptSeconds, or leave activeDeadlineSeconds unset (the default)." (int $deadline) (int $timeToFail) $n (int .Values.migrate.assumedAttemptSeconds) (int $timeToFail)) -}}
{{- end -}}
{{- end -}}
{{- end -}}
