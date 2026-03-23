{{- define "drawbar.serviceAccountName" -}}
{{- if .Values.serviceAccount.name }}
{{- .Values.serviceAccount.name }}
{{- else }}
{{- printf "%s" (include "drawbar.fullname" .) }}
{{- end }}
{{- end }}

{{- define "drawbar.fullname" -}}
{{- printf "%s-%s" .Release.Name "runner" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "drawbar.jobNamespace" -}}
{{- if .Values.runner.jobNamespace }}
{{- .Values.runner.jobNamespace }}
{{- else }}
{{- .Release.Namespace }}
{{- end }}
{{- end }}
