{{- define "zhiyuan.name" -}}{{ .Release.Name }}-{{ .Chart.Name }}{{- end }}

{{- define "zhiyuan.ns" -}}{{ .Values.global.namespaces.main }}{{- end }}
{{- define "zhiyuan.nsMemory" -}}{{ .Values.global.namespaces.memory }}{{- end }}

{{- define "zhiyuan.assertRealValue" -}}
{{- $v := .value -}}
{{- if and (kindIs "string" $v) (regexMatch "<[^<>]+>" $v) -}}
{{- fail (printf "%s still contains a <...> placeholder (%q) — set the real value in values-<env>.local.yaml (gitignored) before installing; placeholders that slip through get written into the cluster and fail later (or break running components on restart)" .name $v) -}}
{{- end -}}
{{- end }}

{{- define "zhiyuan.image" -}}
{{- /* The intranet local repo is flat (no org namespace): strip the ghcr
       org prefix when the registry points at it, so values can keep the
       ghcr-shaped repository on every environment. */ -}}
{{- $repo := .repo -}}
{{- if hasSuffix "/zhiyuan" .registry -}}
{{- $repo = regexReplaceAll "^[^/]+/" .repo "" -}}
{{- end -}}
{{ .registry }}/{{ $repo }}:{{ .tag }}{{ include "zhiyuan.digestSuffix" .digest }}
{{- end }}

{{/* Optional digest pin: "sha256:<64 hex>" renders as "@sha256:..." after
     the tag (repo:tag@digest — the runtime pulls by digest, the tag stays
     for humans). Anything else fails rendering. */}}
{{- define "zhiyuan.digestSuffix" -}}
{{- if . -}}
{{- if not (regexMatch "^sha256:[a-f0-9]{64}$" .) -}}
{{- fail (printf "image digest %q must look like sha256:<64 hex chars>" .) -}}
{{- end -}}
@{{ . }}
{{- end -}}
{{- end }}

{{/* Fully-qualified image values ({repository, tag, digest?}) that do not go
     through global.registry. */}}
{{- define "zhiyuan.rawImage" -}}
{{ .repository }}:{{ .tag }}{{ include "zhiyuan.digestSuffix" .digest }}
{{- end }}
