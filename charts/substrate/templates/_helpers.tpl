{{/*
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/}}

{{/*
Qualified resource name for a chart component.

Usage:
  {{ include "substrate.fullname" (list "ate-api-server" .) }}

When the release name equals the chart name (the canonical render in
hack/render-manifests.sh — `helm template substrate charts/substrate`), this
returns the bare component name, so the generated manifests/ate-install/
files keep their historical names ("ate-api-server", "ate-controller", ...).

Otherwise resources are prefixed with the release name in the standard Helm
style ("foo-ate-api-server", ...) so multiple releases coexist without
colliding.
*/}}
{{- define "substrate.fullname" -}}
{{- $name := index . 0 -}}
{{- $ctx := index . 1 -}}
{{- if eq $ctx.Release.Name $ctx.Chart.Name -}}
{{- $name -}}
{{- else -}}
{{- printf "%s-%s" $ctx.Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
Build an image reference for a substrate component binary.

Usage:
  {{ include "substrate.componentImage" (list "ateapi" .) }}

Produces  {image.registry}/{name}:{tag}  where tag is resolved as:
  1. image.tag value, if set and not the sentinel "<none>"
  2. .Chart.AppVersion, if image.tag is empty
  3. no tag (no colon) when image.tag is the sentinel "<none>"

The "<none>" sentinel is used by hack/render-manifests.sh so that ko:// refs
are emitted without a tag, letting `ko resolve` supply the digest at build time.
*/}}
{{- define "substrate.componentImage" -}}
{{- $name := index . 0 -}}
{{- $ctx := index . 1 -}}
{{- $registry := $ctx.Values.image.registry -}}
{{- $tag := $ctx.Values.image.tag | default $ctx.Chart.AppVersion -}}
{{- if ne $tag "<none>" -}}
{{- printf "%s/%s:%s" $registry $name $tag -}}
{{- else -}}
{{- printf "%s/%s" $registry $name -}}
{{- end -}}
{{- end -}}

{{/*
Validate auth.mode at template time.
*/}}
{{- define "substrate.validateAuthMode" -}}
{{- if not (or (eq .Values.auth.mode "mtls") (eq .Values.auth.mode "jwt")) -}}
{{- fail (printf "auth.mode must be 'mtls' or 'jwt', got %q" .Values.auth.mode) -}}
{{- end -}}
{{- if eq .Values.auth.mode "jwt" -}}
{{- if not .Values.auth.jwt.issuer -}}
{{- fail "auth.jwt.issuer is required when auth.mode=jwt" -}}
{{- end -}}
{{- end -}}
{{- end -}}
