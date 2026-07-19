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

package artifact

import (
	"regexp"
	"strings"
)

var (
	bearerPattern = regexp.MustCompile(`(?i)(authorization\s*[:=]\s*bearer|bearer)\s+[^\s,;]+`)
	queryPattern  = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://[^\s?#]+)(?:\?[^\s#]*)?(?:#[^\s]*)?`)
	tokenPattern  = regexp.MustCompile(`(?i)(token|secret|signature|credential|password)(\s*[:=]\s*)[^\s,;]+`)
)

// Sanitize removes credentials and query strings from an externally visible
// message. Callers should still avoid constructing errors with secret material.
func Sanitize(message string, secrets ...string) string {
	message = bearerPattern.ReplaceAllString(message, "$1 [REDACTED]")
	message = tokenPattern.ReplaceAllString(message, "$1$2[REDACTED]")
	message = queryPattern.ReplaceAllString(message, "$1?[REDACTED]")
	for _, secret := range secrets {
		secret = strings.TrimSpace(secret)
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	return message
}
