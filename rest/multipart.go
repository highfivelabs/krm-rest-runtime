// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

package rest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"sort"
	"strings"
)

// EncodeMultipart encodes fields as multipart/form-data, returning the
// Content-Type header (with boundary) and body bytes. Fields named in
// fileFields hold either a single file object ({name, content, contentBase64,
// contentType}) or a slice of them; other object/array fields are encoded as
// application/json parts, and scalars as plain form fields.
func EncodeMultipart(fields map[string]any, fileFields []string) (string, []byte, error) {
	isFile := make(map[string]bool, len(fileFields))
	for _, f := range fileFields {
		isFile[f] = true
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, k := range keys {
		v := fields[k]
		if v == nil {
			continue
		}
		if isFile[k] {
			if err := writeFileParts(mw, k, v); err != nil {
				return "", nil, err
			}
			continue
		}
		switch v.(type) {
		case map[string]any, []any:
			if err := writeJSONPart(mw, k, v); err != nil {
				return "", nil, err
			}
		default:
			if err := mw.WriteField(k, fmt.Sprint(v)); err != nil {
				return "", nil, err
			}
		}
	}
	if err := mw.Close(); err != nil {
		return "", nil, err
	}
	return mw.FormDataContentType(), buf.Bytes(), nil
}

func writeFileParts(mw *multipart.Writer, field string, v any) error {
	switch tv := v.(type) {
	case map[string]any:
		return writeOneFilePart(mw, field, tv)
	case []any:
		for i, elem := range tv {
			m, ok := elem.(map[string]any)
			if !ok {
				return fmt.Errorf("multipart field %s[%d]: expected file object, got %T", field, i, elem)
			}
			if err := writeOneFilePart(mw, field, m); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("multipart field %s: expected file object or list, got %T", field, v)
	}
}

func writeOneFilePart(mw *multipart.Writer, field string, file map[string]any) error {
	name, _ := file["name"].(string)
	if name == "" {
		name = field
	}
	ct, _ := file["contentType"].(string)
	if ct == "" {
		ct = "application/octet-stream"
	}
	var data []byte
	if b64, _ := file["contentBase64"].(string); b64 != "" {
		var err error
		data, err = base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return fmt.Errorf("multipart file %q: invalid contentBase64: %w", name, err)
		}
	} else {
		s, _ := file["content"].(string)
		data = []byte(s)
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, escapeQuotes(field), escapeQuotes(name)))
	h.Set("Content-Type", ct)
	pw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = pw.Write(data)
	return err
}

func writeJSONPart(mw *multipart.Writer, field string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"`, escapeQuotes(field)))
	h.Set("Content-Type", "application/json")
	pw, err := mw.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = pw.Write(b)
	return err
}

var quoteEscaper = strings.NewReplacer("\\", "\\\\", `"`, "\\\"")

func escapeQuotes(s string) string {
	return quoteEscaper.Replace(s)
}
