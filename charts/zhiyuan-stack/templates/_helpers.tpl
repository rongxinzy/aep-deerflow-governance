{{- define "zhiyuan.name" -}}{{ .Release.Name }}-{{ .Chart.Name }}{{- end }}

{{- define "zhiyuan.ns" -}}{{ .Values.global.namespaces.main }}{{- end }}
{{- define "zhiyuan.nsMemory" -}}{{ .Values.global.namespaces.memory }}{{- end }}

{{- define "zhiyuan.image" -}}
{{- /* The intranet local repo is flat (no org namespace): strip the ghcr
       org prefix when the registry points at it, so values can keep the
       ghcr-shaped repository on every environment. */ -}}
{{- $repo := .repo -}}
{{- if hasSuffix "/zhiyuan" .registry -}}
{{- $repo = regexReplaceAll "^[^/]+/" .repo "" -}}
{{- end -}}
{{ .registry }}/{{ $repo }}:{{ .tag }}
{{- end }}
