{{- /* Generate config map data */}}
{{- define "tinkerbell.configData" -}}
{{- $values := .Values.deployment.envs -}}
{{- range $kk, $vv := $values }}
{{- if kindIs "map" $vv }}
{{- range $k, $v := $vv }}
{{- $key := (list "TINKERBELL" (ternary "" (snakecase $kk | upper) (eq (upper $kk) "GLOBALS")) (snakecase $k | upper) | compact | join "_") }}
{{- if kindIs "invalid" $v }}
{{ $key }}:
{{- else if kindIs "map" $v }}
{{ $key }}: {{ $v | toJson | quote }}
{{- else if kindIs "slice" $v }}
{{- if and (eq $kk "ipxe") (eq $k "httpScriptExtraKernelArgs") }}
{{ $key }}: {{ join " " (append $v (printf "tink_worker_image=%s:%s" $.Values.deployment.agentImage (coalesce $.Values.deployment.agentImageTag $.Chart.AppVersion))) | quote }}
{{- else }}
{{ $key }}: {{ join "," $v | quote }}
{{- end }}
{{- else if kindIs "string" $v }}
{{- if $v }}
{{ $key }}: {{ tpl $v $ | quote }}
{{- end }}
{{- else }}
{{ $key }}: {{ $v | quote }}
{{- end }}
{{- end }}
{{- end }}
{{- end }}
{{- end -}}
