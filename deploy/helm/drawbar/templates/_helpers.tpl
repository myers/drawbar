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

{{- /*
Convert runner.shutdownTimeout (Go duration string) to integer seconds and
add a 5-second buffer for the kubelet's SIGKILL after drain completes.
Supports a single-unit suffix: "Ns", "Nm", or "Nh". Mixed forms ("1h30m")
are accepted by the controller's time.ParseDuration but not by this helper —
use seconds (e.g. "5400s" for 1.5h) for compound durations.
*/ -}}
{{- define "drawbar.shutdownGraceSeconds" -}}
{{- $d := .Values.runner.shutdownTimeout | default "60s" -}}
{{- $unit := $d | trimAll "0123456789" -}}
{{- $n := $d | trimSuffix $unit | int -}}
{{- $secs := 0 -}}
{{- if eq $unit "s" }}{{- $secs = $n -}}
{{- else if eq $unit "m" }}{{- $secs = mul $n 60 -}}
{{- else if eq $unit "h" }}{{- $secs = mul $n 3600 -}}
{{- else }}{{- fail (printf "runner.shutdownTimeout %q: unsupported unit %q (use Ns, Nm, or Nh)" $d $unit) -}}
{{- end -}}
{{- add $secs 5 -}}
{{- end -}}
