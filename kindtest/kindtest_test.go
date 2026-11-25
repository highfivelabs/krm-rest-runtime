// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	_ "embed"
	"net/http"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

//go:embed testdata/types.yaml
var typesYAML []byte

func TestEveryShape(t *testing.T) {
	v := &rest.Vendor{Name: "example", CredentialKeys: []string{"token"},
		Authorize: func(r *http.Request, c map[string]string) { r.Header.Set("Authorization", "Bearer "+c["token"]) }}
	EveryKind(t, typesYAML, v, nil)
}

// A kind with no update cannot opt in to replacing unless its descriptor says
// how (Role has no Replace).
func TestRefusesReplace(t *testing.T) {
	v := &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}
	RefusesReplace(t, typesYAML, v, "Role", "Widget")
}

func TestReplaces(t *testing.T) {
	v := &rest.Vendor{Name: "example", CredentialKeys: []string{"token"}}
	Replaces(t, typesYAML, v, "Script", "Function", "Signature")
}
