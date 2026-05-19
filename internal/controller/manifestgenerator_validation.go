/*
Copyright 2026 Travis Vanderpool

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"strings"

	mgapi "github.com/tvanderpool/flux-manifest-generator/api/v1alpha1"
	"github.com/tvanderpool/flux-manifest-generator/internal/pipeline"
)

// validateSpec performs runtime validation that cannot be expressed via
// kubebuilder markers.
//
// Slice-5 scope: sources + artifacts + pipeline (all five verbs).
// values, valuesFrom and per-artifact forEach are still accepted by the
// CRD schema but rejected here because slices 7–8 have not landed yet.
// Tighten / relax these checks in the slice that introduces the
// corresponding feature.
func (r *ManifestGeneratorReconciler) validateSpec(obj *mgapi.ManifestGenerator) error {
	aliasMap := make(map[string]bool, len(obj.Spec.Sources))
	for _, src := range obj.Spec.Sources {
		if aliasMap[src.Alias] {
			return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
				"duplicate source alias %q", src.Alias)
		}
		aliasMap[src.Alias] = true

		if r.NoCrossNamespaceRefs && src.Namespace != "" && src.Namespace != obj.Namespace {
			return r.newTerminalErrorFor(obj, mgapi.AccessDeniedReason,
				"cross-namespace reference to source %s/%s/%s is not allowed",
				src.Kind, src.Namespace, src.Name)
		}
	}

	if len(obj.Spec.Pipeline) > 0 {
		// Compile the pipeline at validation time so syntax errors,
		// unknown aliases, malformed keyExprs, and slice-5+ verbs all
		// surface as terminal validation failures rather than late
		// reconcile errors.
		if _, err := pipeline.Compile(obj.Spec.Pipeline, aliasMap); err != nil {
			return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
				"spec.pipeline: %s", err.Error())
		}
	}
	if obj.Spec.Values != nil {
		return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
			"spec.values is not yet supported in this controller version")
	}
	if len(obj.Spec.ValuesFrom) > 0 {
		return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
			"spec.valuesFrom is not yet supported in this controller version")
	}

	nameMap := make(map[string]bool, len(obj.Spec.Artifacts))
	for i := range obj.Spec.Artifacts {
		artifact := &obj.Spec.Artifacts[i]

		if nameMap[artifact.Name] {
			return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
				"duplicate artifact name %q", artifact.Name)
		}
		nameMap[artifact.Name] = true

		if artifact.ForEach != nil {
			return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
				"artifact %q: forEach is not yet supported in this controller version",
				artifact.Name)
		}

		if artifact.Revision != "" && !aliasMap[strings.TrimPrefix(artifact.Revision, "@")] {
			return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
				"artifact %q: revision source alias %q not found",
				artifact.Name, strings.TrimPrefix(artifact.Revision, "@"))
		}
		if artifact.OriginRevision != "" && !aliasMap[strings.TrimPrefix(artifact.OriginRevision, "@")] {
			return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
				"artifact %q: originRevision source alias %q not found",
				artifact.Name, strings.TrimPrefix(artifact.OriginRevision, "@"))
		}

		for _, t := range artifact.Templates {
			alias, ok := templateSourceAlias(t.From)
			if !ok {
				return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
					"artifact %q: invalid template source %q", artifact.Name, t.From)
			}
			if !aliasMap[alias] {
				return r.newTerminalErrorFor(obj, mgapi.ValidationFailedReason,
					"artifact %q: template references unknown source alias %q",
					artifact.Name, alias)
			}
		}
	}

	return nil
}

// templateSourceAlias extracts the alias from an `@<alias>/<path>`
// reference, returning false for malformed input. CRD-level pattern
// validation already enforces the syntax for valid CRs; this is the
// defensive runtime check.
func templateSourceAlias(ref string) (string, bool) {
	if !strings.HasPrefix(ref, "@") {
		return "", false
	}
	rest := ref[1:]
	idx := strings.IndexByte(rest, '/')
	if idx <= 0 || idx == len(rest)-1 {
		return "", false
	}
	return rest[:idx], true
}
