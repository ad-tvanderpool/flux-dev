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

package builder

import (
	"fmt"
	"text/template"

	mgtemplate "github.com/tvanderpool/flux-manifest-generator/internal/template"
)

// parseDestTemplate verifies that the supplied destination-path
// template parses cleanly under the shared FuncMap. Execution is
// deferred until build time so that runtime values (the per-iteration
// forEach binding, pipeline outputs) are in scope. Parse-only checks
// catch the bulk of typos at validation time without forcing the
// validator to hold an Engine.
func parseDestTemplate(pathTemplate string) error {
	if _, err := template.New("dest").
		Funcs(mgtemplate.FuncMap()).
		Option("missingkey=zero").
		Parse(pathTemplate); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	return nil
}
