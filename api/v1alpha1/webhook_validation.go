/*
Copyright 2026 Kama Authors.

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

package v1alpha1

import (
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
)

const maxArtifactPathLength = 1024

func validateLocalObjectName(name string, fldPath *field.Path) *field.Error {
	if name == "" {
		return field.Required(fldPath, "name is required")
	}
	if messages := validation.IsDNS1123Subdomain(name); len(messages) > 0 {
		return field.Invalid(fldPath, name, strings.Join(messages, "; "))
	}
	return nil
}

func validateMetadataKey(key string, fldPath *field.Path) *field.Error {
	if messages := validation.IsQualifiedName(key); len(messages) > 0 {
		return field.Invalid(fldPath, key, strings.Join(messages, "; "))
	}
	return nil
}

func validateCleanRelativePath(value string, fldPath *field.Path, allowDot, allowGlob bool) *field.Error {
	if value == "" {
		return field.Required(fldPath, "a relative path is required")
	}
	if len(value) > maxArtifactPathLength {
		return field.TooLong(fldPath, value, maxArtifactPathLength)
	}
	if !utf8.ValidString(value) {
		return field.Invalid(fldPath, value, "must be valid UTF-8")
	}
	if strings.ContainsRune(value, '\x00') || strings.Contains(value, "\\") {
		return field.Invalid(fldPath, value, "must use POSIX separators and contain no NUL bytes")
	}
	if strings.HasPrefix(value, "/") || path.IsAbs(value) {
		return field.Invalid(fldPath, value, "must be relative")
	}
	if value == "." {
		if allowDot {
			return nil
		}
		return field.Invalid(fldPath, value, "must name a file below the content root")
	}
	if path.Clean(value) != value {
		return field.Invalid(fldPath, value, "must be a clean relative path without '.', '..', empty, or trailing segments")
	}
	for segment := range strings.SplitSeq(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return field.Invalid(fldPath, value, "must not contain empty, '.' or '..' segments")
		}
	}
	if allowGlob {
		if _, err := path.Match(value, value); err != nil {
			return field.Invalid(fldPath, value, fmt.Sprintf("invalid POSIX file selector: %v", err))
		}
		return nil
	}
	if strings.ContainsAny(value, "*?[") {
		return field.Invalid(fldPath, value, "wildcards are not permitted")
	}
	return nil
}
