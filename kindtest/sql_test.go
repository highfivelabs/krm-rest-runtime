// SPDX-License-Identifier: Apache-2.0

package kindtest

import (
	_ "embed"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

//go:embed testdata/sql/types.yaml
var sqlTypesYAML []byte

// Every sql mode (an object read by SHOW and a DESCRIBE row, one read by
// DESCRIBE's property rows, a setting, a grant) through create, read back,
// update, drift repair and delete on the fake's SQL API.
func TestEverySQLKind(t *testing.T) {
	v := &rest.Vendor{Name: "example", CredentialKeys: []string{"token"},
		Authorize: func(r *http.Request, c map[string]string) { r.Header.Set("Authorization", "Bearer "+c["token"]) }}
	EveryKind(t, sqlTypesYAML, v, nil)
}

// The live steps of every sql kind, recorded against the fake and replayed:
// the statements the runtime sends are the cassettes' (testdata/sql/cassettes,
// recorded again with KRM_FAKE_RECORD=1), so a change in them fails here.
func TestRehearseSQLKinds(t *testing.T) {
	t.Setenv(EnvLiveRecords, filepath.Join(t.TempDir(), "records.json"))
	t.Setenv(EnvLiveCassettes, filepath.Join("testdata", "sql", "cassettes"))
	Rehearse(t, sqlTypesYAML, exampleVendor(), nil, nil)
}
