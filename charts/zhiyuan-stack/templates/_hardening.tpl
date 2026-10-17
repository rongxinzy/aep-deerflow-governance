{{/* 加固模板共用片段（NetworkPolicy / PSA）。 */}}

{{- define "zhiyuan.np.monitoringNs" -}}
{{- default .Values.observability.monitoringNamespace .Values.networkPolicy.monitoringNamespace -}}
{{- end }}

{{/* 运行时 Pod 选择器：有员工名标签、且不是 a2a 适配器。operator 升级出
     component 标签前，运行时 Pod 没有该标签，NotIn 依然命中。 */}}
{{- define "zhiyuan.np.runtimeSelector" -}}
matchExpressions:
  - {key: digitalemployee.zhiyuan.io/name, operator: Exists}
  - {key: digitalemployee.zhiyuan.io/component, operator: NotIn, values: [a2a]}
{{- end }}

{{- define "zhiyuan.np.fromNamespace" -}}
- namespaceSelector:
    matchLabels: {kubernetes.io/metadata.name: {{ . }}}
{{- end }}

{{- define "zhiyuan.np.defaultDenyIngress" -}}
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: zhiyuan-default-deny-ingress
  namespace: {{ . }}
spec:
  podSelector: {}
  policyTypes: [Ingress]
{{- end }}

{{- define "zhiyuan.psa.labels" -}}
{{- $levels := .levels -}}
pod-security.kubernetes.io/enforce: {{ $levels.enforce }}
pod-security.kubernetes.io/enforce-version: {{ .version }}
pod-security.kubernetes.io/warn: {{ $levels.warn }}
pod-security.kubernetes.io/warn-version: {{ .version }}
pod-security.kubernetes.io/audit: {{ $levels.audit }}
pod-security.kubernetes.io/audit-version: {{ .version }}
{{- end }}

{{/* 命名空间标签：Helm 归属 + PSA（podSecurity.enabled 且该命名空间配置了级别时）。
     主命名空间在 workspaceAgent.enabled 时 enforce 提升为 privileged：
     pw-node-agent 为特权容器 + hostNetwork + hostPath，baseline 会拒绝。 */}}
{{- define "zhiyuan.nsLabels" -}}
{{- $root := .root -}}
app.kubernetes.io/managed-by: {{ $root.Release.Service }}
app.kubernetes.io/instance: {{ $root.Release.Name }}
{{- $ps := $root.Values.podSecurity -}}
{{- if and $ps.enabled (hasKey $ps.levels .key) }}
{{- $levels := deepCopy (index $ps.levels .key) -}}
{{- if and (eq .key "main") $root.Values.workspaceAgent.enabled }}
{{- $_ := set $levels "enforce" "privileged" -}}
{{- end }}
{{ include "zhiyuan.psa.labels" (dict "levels" $levels "version" $ps.version) }}
{{- end }}
{{- end }}
