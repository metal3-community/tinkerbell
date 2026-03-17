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
{{ $key }}: {{ join "," $v | quote }}
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
