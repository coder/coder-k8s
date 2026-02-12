{{- define "fieldDoc" -}}
{{- $doc := . -}}
{{- if not $doc -}}
{{- "" -}}
{{- else -}}
{{- $rendered := markdownRenderFieldDoc $doc -}}
{{- $rendered = $rendered | replace "<br />" " " -}}
{{- $rendered = regexReplaceAll "(https?://[^[:space:]<]+)" $rendered "[$1]($1)" -}}
{{- $rendered -}}
{{- end -}}
{{- end -}}

{{- define "renderFieldsTableRows" -}}
{{- range $field := . -}}
{{- if not $field.Type }}{{ fail (printf "field %q is missing type information" $field.Name) }}{{- end -}}
{{- $fieldDoc := markdownRenderFieldDoc $field.Doc -}}
{{- $fieldDoc = $fieldDoc | replace "<br />" " " -}}
{{- $fieldDoc = regexReplaceAll "(https?://[^[:space:]<]+)" $fieldDoc "[$1]($1)" -}}
| `{{ $field.Name }}` | {{ markdownRenderType $field.Type }} | {{ $fieldDoc }} |
{{ end -}}
{{- end -}}

{{- define "collectReferencedTypes" -}}
{{- $type := .type -}}
{{- if not $type -}}
{{- fail "assertion failed: collectReferencedTypes requires type" -}}
{{- end -}}
{{- $refs := .refs -}}
{{- $visited := .visited -}}

{{- $uid := $type.UID -}}
{{- if eq $uid "" -}}
{{- $uid = $type.String -}}
{{- end -}}
{{- if not (hasKey $visited $uid) -}}
{{- $_ := set $visited $uid true -}}

{{- $members := $type.Members -}}
{{- $typePackage := $type.Package -}}
{{- $isProjectLocal := or (hasPrefix "github.com/coder/coder-k8s/api/v1alpha1" $typePackage) (hasPrefix "github.com/coder/coder-k8s/api/aggregation/v1alpha1" $typePackage) -}}
{{- if and $isProjectLocal (or (gt (len $members) 0) (gt (len $type.EnumValues) 0)) -}}
{{- $typeKey := printf "%s.%s" $type.Package $type.Name -}}
{{- $_ := set $refs $typeKey $type -}}
{{- end -}}

{{- if $type.UnderlyingType -}}
{{- template "collectReferencedTypes" (dict "type" $type.UnderlyingType "refs" $refs "visited" $visited) -}}
{{- end -}}
{{- if $type.KeyType -}}
{{- template "collectReferencedTypes" (dict "type" $type.KeyType "refs" $refs "visited" $visited) -}}
{{- end -}}
{{- if $type.ValueType -}}
{{- template "collectReferencedTypes" (dict "type" $type.ValueType "refs" $refs "visited" $visited) -}}
{{- end -}}
{{- range $field := $members -}}
{{- if not $field.Type }}{{ fail (printf "field %q is missing type information" $field.Name) }}{{- end -}}
{{- template "collectReferencedTypes" (dict "type" $field.Type "refs" $refs "visited" $visited) -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "renderTypeDefinition" -}}
{{- $type := . -}}
{{- if not $type -}}
{{- fail "assertion failed: renderTypeDefinition requires type" -}}
{{- end -}}
### {{ $type.Name }}

{{ $typeDoc := trim $type.Doc }}
{{ if ne $typeDoc "" }}
{{ regexReplaceAll "(https?://[^[:space:]<]+)" $typeDoc "[$1]($1)" }}

{{ end }}
{{ if gt (len $type.Members) 0 }}
| Field | Type | Description |
| --- | --- | --- |
{{ template "renderFieldsTableRows" $type.Members }}

{{ end }}
{{ if gt (len $type.EnumValues) 0 }}
| Value | Description |
| --- | --- |
{{ range $value := $type.EnumValues }}
| `{{ $value.Name }}` | {{ template "fieldDoc" $value.Doc }} |
{{ end }}

{{ end }}
{{- end -}}

{{- define "gvList" -}}
{{- $groupVersions := . -}}
{{- if ne (len $groupVersions) 1 -}}
{{ fail "expected exactly one group-version from --source-path" }}
{{- end -}}

{{- $kind := markdownTemplateValue "kind" -}}
{{- if eq $kind "" -}}
{{ fail "missing --template-value=kind=<Kind>" }}
{{- end -}}

{{- $goType := markdownTemplateValue "goType" -}}
{{- if eq $goType "" -}}
{{ fail "missing --template-value=goType=<path>" }}
{{- end -}}

{{- $scope := markdownTemplateValue "scope" -}}
{{- if eq $scope "" -}}
{{- $scope = "namespaced" -}}
{{- end -}}

{{- $resource := markdownTemplateValue "resource" -}}
{{- if eq $resource "" -}}
{{- $resource = printf "%ss" (lower $kind) -}}
{{- end -}}

{{- $gv := index $groupVersions 0 -}}
{{- $rootType := $gv.TypeForKind $kind -}}
{{- if not $rootType -}}
{{ fail (printf "kind %q not found in source tree" $kind) }}
{{- end -}}
{{- if not $rootType.GVK -}}
{{ fail (printf "kind %q is missing GVK metadata" $kind) }}
{{- end -}}

{{- $specMembers := list -}}
{{- $statusMembers := list -}}
{{- range $field := $rootType.Members -}}
{{- if eq $field.Name "spec" -}}
{{- $specMembers = $field.Type.Members -}}
{{- end -}}
{{- if eq $field.Name "status" -}}
{{- $statusMembers = $field.Type.Members -}}
{{- end -}}
{{- end -}}
{{- if eq (len $specMembers) 0 -}}
{{ fail (printf "expected kind %q to contain at least one spec field" $kind) }}
{{- end -}}
{{- if eq (len $statusMembers) 0 -}}
{{ fail (printf "expected kind %q to contain at least one status field" $kind) }}
{{- end -}}

{{- $referencedTypes := dict -}}
{{- $visitedTypes := dict -}}
{{- range $field := $specMembers -}}
{{- if not $field.Type }}{{ fail (printf "field %q is missing type information" $field.Name) }}{{- end -}}
{{- template "collectReferencedTypes" (dict "type" $field.Type "refs" $referencedTypes "visited" $visitedTypes) -}}
{{- end -}}
{{- range $field := $statusMembers -}}
{{- if not $field.Type }}{{ fail (printf "field %q is missing type information" $field.Name) }}{{- end -}}
{{- template "collectReferencedTypes" (dict "type" $field.Type "refs" $referencedTypes "visited" $visitedTypes) -}}
{{- end -}}
{{- $referencedTypeKeys := keys $referencedTypes | sortAlpha -}}

<!-- Code generated by hack/update-reference-docs.sh using github.com/elastic/crd-ref-docs. DO NOT EDIT. -->

# `{{ $kind }}`

## API identity

- Group/version: `{{ $rootType.GVK.Group }}/{{ $rootType.GVK.Version }}`
- Kind: `{{ $rootType.GVK.Kind }}`
- Resource: `{{ $resource }}`
- Scope: {{ $scope }}

## Spec

| Field | Type | Description |
| --- | --- | --- |
{{ template "renderFieldsTableRows" $specMembers }}

## Status

| Field | Type | Description |
| --- | --- | --- |
{{ template "renderFieldsTableRows" $statusMembers }}

{{ if gt (len $referencedTypeKeys) 0 }}
## Referenced types

{{ range $typeKey := $referencedTypeKeys }}
{{ $type := index $referencedTypes $typeKey }}
{{ template "renderTypeDefinition" $type }}
{{ end }}
{{ end }}

## Source

- Go type: `{{ $goType }}`
{{- $generatedCRD := markdownTemplateValue "generatedCRD" -}}
{{ if ne $generatedCRD "" }}
- Generated CRD: `{{ $generatedCRD }}`
{{ end }}
{{- $storage := markdownTemplateValue "storage" -}}
{{ if ne $storage "" }}
- Storage implementation: `{{ $storage }}`
{{ end }}
{{- $apiServiceManifest := markdownTemplateValue "apiServiceManifest" -}}
{{ if ne $apiServiceManifest "" }}
- APIService registration manifest: `{{ $apiServiceManifest }}`
{{ end }}
{{- end -}}
