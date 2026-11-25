// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEncodeMultipartAndDoWith(t *testing.T) {
	var gotParts []struct {
		name, filename, ct string
		data               []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/javascript")
			_, _ = w.Write([]byte("export default {};"))
			return
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("ParseMediaType: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatalf("NextPart: %v", err)
			}
			b, _ := io.ReadAll(p)
			gotParts = append(gotParts, struct {
				name, filename, ct string
				data               []byte
			}{p.FormName(), p.FileName(), p.Header.Get("Content-Type"), b})
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":{"id":"s1"}}`))
	}))
	defer srv.Close()

	c := New(&Vendor{Name: "v"}, srv.URL, nil)
	mp := &MultipartBody{
		Files: []string{"files"},
		Fields: map[string]any{
			"metadata": map[string]any{"main_module": "index.js"},
			"branch":   "main",
			"files": []any{
				map[string]any{
					"name":        "index.js",
					"contentType": "application/javascript+module",
					"content":     "export default {};",
				},
				map[string]any{
					"name":          "data.bin",
					"contentBase64": base64.StdEncoding.EncodeToString([]byte{0x00, 0x01, 0x02}),
				},
			},
		},
	}
	resp, err := c.DoWith(context.Background(), http.MethodPut, "/scripts/s1", mp, nil)
	if err != nil {
		t.Fatalf("PUT multipart: %v", err)
	}
	if resp == nil {
		t.Fatalf("PUT multipart: nil response")
	}
	if len(gotParts) != 4 {
		t.Fatalf("got %d parts, want 4: %+v", len(gotParts), gotParts)
	}
	// Sorted keys: branch, files (x2), metadata.
	if gotParts[0].name != "branch" || string(gotParts[0].data) != "main" {
		t.Errorf("part 0 = %+v", gotParts[0])
	}
	if gotParts[1].name != "files" || gotParts[1].filename != "index.js" || gotParts[1].ct != "application/javascript+module" || string(gotParts[1].data) != "export default {};" {
		t.Errorf("part 1 = %+v", gotParts[1])
	}
	if gotParts[2].name != "files" || gotParts[2].filename != "data.bin" || gotParts[2].ct != "application/octet-stream" || !bytes.Equal(gotParts[2].data, []byte{0x00, 0x01, 0x02}) {
		t.Errorf("part 2 = %+v", gotParts[2])
	}
	var meta map[string]any
	if err := json.Unmarshal(gotParts[3].data, &meta); err != nil || gotParts[3].name != "metadata" || gotParts[3].ct != "application/json" || meta["main_module"] != "index.js" {
		t.Errorf("part 3 = %+v", gotParts[3])
	}

	// Non-JSON response on GET returns nil without error.
	getResp, err := c.DoWith(context.Background(), http.MethodGet, "/scripts/s1", nil, nil)
	if err != nil || getResp != nil {
		t.Errorf("GET non-json = (%v, %v), want (nil, nil)", getResp, err)
	}
}
