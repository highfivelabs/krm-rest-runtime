// SPDX-License-Identifier: Apache-2.0

package rest

import "regexp"

var placeholderRE = regexp.MustCompile(`\{[^}]+\}`)
