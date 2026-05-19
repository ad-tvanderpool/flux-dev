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

package pipeline

import (
	"fmt"
	"sort"
)

// listInput coerces the named step's output into a []any. Returns a
// descriptive error if the value isn't a list, so verb implementations
// can wrap with their verb / step context.
func listInput(outputs Outputs, from string) ([]any, error) {
	val, ok := outputs[from]
	if !ok {
		return nil, fmt.Errorf("from %q is not defined", from)
	}
	list, ok := val.([]any)
	if !ok {
		return nil, fmt.Errorf("from %q is not a list (got %T)", from, val)
	}
	return list, nil
}

// sortedKeys returns the keys of m in lexicographic order. Used by
// every map-iterating verb so identical inputs always yield identical
// output ordering even though Go's map iteration is randomised.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
