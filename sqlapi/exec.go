// SPDX-License-Identifier: Apache-2.0

package sqlapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/highfivelabs/krm-rest-runtime/rest"
)

// Timeout is how long Snowflake may run one statement (the request's
// timeout, in seconds) and how long Exec polls one that answered 202.
var Timeout = 60 * time.Second

// Request is the body of POST /api/v2/statements (sqlapi.yaml's
// SubmitStatement): one statement, and a parameter that refuses more.
type Request struct {
	Statement  string         `json:"statement"`
	Timeout    int            `json:"timeout"`
	Parameters map[string]any `json:"parameters"`
	// Warehouse runs the statement (as given: the SQL API reads it
	// case-sensitively); none, the user's default warehouse.
	Warehouse string `json:"warehouse,omitempty"`
}

// Result is a statement's answer: its columns' names and its rows, every
// value text or nil (null).
type Result struct {
	Columns []string
	Rows    [][]*string
}

// Row is one row by column name.
func (r *Result) Row(i int) map[string]*string {
	out := make(map[string]*string, len(r.Columns))
	for j, c := range r.Columns {
		if j < len(r.Rows[i]) {
			out[c] = r.Rows[i][j]
		}
	}
	return out
}

// Runner sends statements to one connection's SQL API.
type Runner struct {
	API *rest.Client
	// Endpoint is the SQL API's path, /api/v2/statements.
	Endpoint string
	// Wait waits between polls of a statement still running (nil: sleep).
	// A replay of recorded calls waits for nothing.
	Wait func(ctx context.Context, d time.Duration) error
}

// response is ResultSet, QueryStatus and QueryFailureStatus in one.
type response struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	StatementHandle   string `json:"statementHandle"`
	ResultSetMetaData *struct {
		RowType []struct {
			Name string `json:"name"`
		} `json:"rowType"`
		PartitionInfo []json.RawMessage `json:"partitionInfo"`
	} `json:"resultSetMetaData"`
	Data [][]*string `json:"data"`
}

// SQLError is a statement Snowflake refused (HTTP 422): its error code and
// message. A message saying the object does not exist (as Terraform's
// provider reads them) is also rest.ErrNotFound.
type SQLError struct {
	Code    string
	Message string
	what    string
}

func (e *SQLError) Error() string {
	return fmt.Sprintf("%s: Snowflake error %s: %s", e.what, e.Code, e.Message)
}

// Is makes a "does not exist" error rest.ErrNotFound.
func (e *SQLError) Is(target error) bool {
	return target == rest.ErrNotFound && NotFoundMessage(e.Message)
}

// NotFoundMessage reports whether an error message says the object does not
// exist: Snowflake's "does not exist or not authorized" and "Object does
// not exist, or operation cannot be performed".
func NotFoundMessage(msg string) bool {
	return strings.Contains(msg, "does not exist or not authorized") ||
		strings.Contains(msg, "Object does not exist, or operation cannot be performed")
}

// What names a statement in errors by its leading keywords (CREATE MASKING
// POLICY), never its values, which may be secrets.
func What(stmt string) string {
	var words []string
	for _, w := range strings.Fields(stmt) {
		if !keywordRE.MatchString(w) || len(words) == 4 {
			break
		}
		words = append(words, w)
	}
	return strings.Join(words, " ")
}

// Exec sends one statement and returns its whole answer: it polls one still
// running (202) until it ends, and reads every partition of a long answer.
func (r *Runner) Exec(ctx context.Context, stmt string) (*Result, error) {
	return r.ExecIn(ctx, stmt, "")
}

// ExecIn is Exec in a warehouse ("" for the user's default), named as the
// request's warehouse takes it: Display of its exact name.
func (r *Runner) ExecIn(ctx context.Context, stmt, warehouse string) (*Result, error) {
	what := What(stmt)
	body := &Request{Statement: stmt, Timeout: int(Timeout / time.Second), Parameters: map[string]any{"multi_statement_count": 1}, Warehouse: warehouse}
	raw, err := r.API.Do(ctx, "POST", r.Endpoint+"?nullable=true", body)
	resp, err := decode(raw, err, what)
	if err != nil {
		return nil, err
	}
	step := 250 * time.Millisecond
	for waited := time.Duration(0); resp.ResultSetMetaData == nil; {
		if resp.StatementHandle == "" {
			return nil, fmt.Errorf("%s: the SQL API answered neither a result nor a statement handle", what)
		}
		if waited >= Timeout {
			return nil, fmt.Errorf("%s: still running after %s (statement %s)", what, Timeout, resp.StatementHandle)
		}
		if err := r.wait(ctx, step); err != nil {
			return nil, err
		}
		waited += step
		step = min(2*step, 5*time.Second)
		raw, err = r.API.Do(ctx, "GET", r.Endpoint+"/"+url.PathEscape(resp.StatementHandle), nil)
		if resp, err = decode(raw, err, what); err != nil {
			return nil, err
		}
	}
	out := &Result{Rows: resp.Data}
	for _, c := range resp.ResultSetMetaData.RowType {
		out.Columns = append(out.Columns, c.Name)
	}
	for p := 1; p < len(resp.ResultSetMetaData.PartitionInfo); p++ {
		raw, err := r.API.Do(ctx, "GET", r.Endpoint+"/"+url.PathEscape(resp.StatementHandle)+"?partition="+strconv.Itoa(p), nil)
		part, err := decode(raw, err, what)
		if err != nil {
			return nil, err
		}
		out.Rows = append(out.Rows, part.Data...)
	}
	return out, nil
}

func decode(raw any, err error, what string) (*response, error) {
	var apiErr *rest.APIError
	if errors.As(err, &apiErr) && apiErr.Status == 422 {
		var fail response
		_ = json.Unmarshal([]byte(apiErr.Body), &fail)
		return nil, &SQLError{Code: fail.Code, Message: fail.Message, what: what}
	}
	if errors.Is(err, rest.ErrNotFound) {
		// The endpoint or a statement handle, not the object: only
		// Snowflake's own message says an object does not exist.
		return nil, fmt.Errorf("%s: the SQL API answered 404", what)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	var resp response
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("%s: decoding the SQL API's answer: %w", what, err)
	}
	return &resp, nil
}

func (r *Runner) wait(ctx context.Context, d time.Duration) error {
	if r.Wait != nil {
		return r.Wait(ctx, d)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}
