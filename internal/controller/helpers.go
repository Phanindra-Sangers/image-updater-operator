/*
Copyright 2026.

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
	"strconv"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// listItems extracts the items of a client.ObjectList as []client.Object.
func listItems(list client.ObjectList) []client.Object {
	runtimeObjs, err := meta.ExtractList(list)
	if err != nil {
		return nil
	}
	out := make([]client.Object, 0, len(runtimeObjs))
	for _, o := range runtimeObjs {
		if co, ok := o.(client.Object); ok {
			out = append(out, co)
		}
	}
	return out
}

type noMatchError struct {
	repo    string
	scanned int
}

func (e noMatchError) Error() string {
	return "no tag matched the policy for " + e.repo + " (" + strconv.Itoa(e.scanned) + " tags scanned)"
}

type missingDockerConfigError struct {
	secret string
}

func (e missingDockerConfigError) Error() string {
	return "secret " + e.secret + " has no .dockerconfigjson or .dockercfg key"
}

func intToStr(i int) string { return strconv.Itoa(i) }
