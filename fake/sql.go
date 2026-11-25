// SPDX-License-Identifier: Apache-2.0

package fake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/sqlapi"
)

// The fake's SQL API answers exactly the statements the sql kinds'
// templates make (descriptor.SQL), read back with the same templates: a
// statement no template makes is refused as a syntax error. It keeps each
// object, setting and grant by its kind and name, and answers a SHOW or
// DESCRIBE with the rows the kind's reads map, every value as text, as the
// SQL API does. A create answers 202 first, so that polling a statement
// still running is proven too.

type sqlKind struct {
	k      *descriptor.Kind
	create *sqlapi.Pattern
	delete *sqlapi.Pattern
	reads  []*sqlapi.Pattern
	alter  map[string]*sqlapi.Pattern
	unset  map[string]*sqlapi.Pattern
	add    map[string]*sqlapi.Pattern
	remove map[string]*sqlapi.Pattern
	// teardown: a settle's teardown statements, by step (nil: a wait).
	teardown []*sqlapi.Pattern
}

type sqlRecord struct {
	kind *descriptor.Kind
	id   []string // an object's exact name
	vals map[string]any
	// An asynchronous object (SQL.Settle) in a transition: its status
	// field shows a transitional status for settleLeft more reads that
	// answer it, then settleTo.
	settleLeft int
	settleTo   string
}

type sqlState struct {
	endpoint string
	kinds    []*sqlKind
	records  map[string]*sqlRecord
	pending  map[string]any // statement handle -> its answer, once polled
	handles  int
	made     int // objects made: what Snowflake makes for each is told apart
}

func newSQLState(f *descriptor.File) *sqlState {
	st := &sqlState{records: map[string]*sqlRecord{}, pending: map[string]any{}}
	for i := range f.Kinds {
		k := &f.Kinds[i]
		if k.SQL == nil {
			continue
		}
		st.endpoint = k.Read.Path
		q := k.SQL
		must := func(t string) *sqlapi.Pattern {
			if t == "" {
				return nil // none: a set kind set field by field has no create and no delete
			}
			p, err := sqlapi.Compile(q, t)
			if err != nil {
				panic(fmt.Sprintf("%s: %v", k.Kind, err))
			}
			return p
		}
		sk := &sqlKind{k: k, create: must(q.Create), delete: must(q.Delete), alter: map[string]*sqlapi.Pattern{}, unset: map[string]*sqlapi.Pattern{},
			add: map[string]*sqlapi.Pattern{}, remove: map[string]*sqlapi.Pattern{}}
		for _, r := range q.Read {
			sk.reads = append(sk.reads, must(r.Statement))
		}
		for f, t := range q.Alter {
			sk.alter[f] = must(t)
		}
		for f, t := range q.Unset {
			sk.unset[f] = must(t)
		}
		for f, t := range q.Add {
			sk.add[f] = must(t)
		}
		for f, t := range q.Remove {
			sk.remove[f] = must(t)
		}
		if q.Settle != nil {
			for _, step := range q.Settle.Teardown {
				var p *sqlapi.Pattern
				if step.Run != "" {
					p = must(step.Run)
				}
				sk.teardown = append(sk.teardown, p)
			}
		}
		st.kinds = append(st.kinds, sk)
		if q.PerField() {
			st.preset(sk)
		}
	}
	if len(st.kinds) == 0 {
		return nil
	}
	return st
}

// preset holds what a set kind set field by field is on, as Snowflake
// always has it (the account; the organization account, named as the
// kind's sample names it): a record with the sample's fields no statement
// sets.
func (st *sqlState) preset(sk *sqlKind) {
	q := sk.k.SQL
	rec := &sqlRecord{kind: sk.k, vals: map[string]any{}}
	for f, v := range sk.k.Sample {
		fd, ok := q.Fields[f]
		if !ok || fd.Output || q.Settable(f) {
			continue
		}
		if s, isStr := v.(string); isStr && fd.Type == "identifier" {
			if exact, err := sqlapi.Ident(s); err == nil {
				v = exact
			}
		}
		rec.vals[f] = v
	}
	st.records[st.key(sk, rec.vals, nil)] = rec
}

// SQLRecords is what the fake's SQL API holds of a kind: each object's,
// setting's or grant's fields, as Snowflake keeps them (identifiers by
// their exact names). The maps are the fake's own: a test may change one
// behind the controller's back.
func (s *Server) SQLRecords(kind string) []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sql == nil {
		return nil
	}
	var keys []string
	for key, r := range s.sql.records {
		if r.kind.Kind == kind {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var out []map[string]any
	for _, key := range keys {
		out = append(out, s.sql.records[key].vals)
	}
	return out
}

func (s *Server) serveSQL(w http.ResponseWriter, req *http.Request) bool {
	st := s.sql
	if st == nil {
		return false
	}
	path := req.URL.Path
	switch {
	case req.Method == http.MethodPost && path == st.endpoint:
		s.Calls["POST "+st.endpoint]++
		data, _ := io.ReadAll(req.Body)
		var body sqlapi.Request
		if err := json.Unmarshal(data, &body); err != nil || body.Statement == "" {
			sqlFail(w, http.StatusBadRequest, "390142", "Incoming request does not contain a valid payload.", "")
			return true
		}
		if n, ok := body.Parameters["multi_statement_count"].(float64); !ok || n != 1 {
			sqlFail(w, 422, "000008", "Actual statement count did not match the desired statement count.", "")
			return true
		}
		st.handles++
		handle := fmt.Sprintf("01c0ffee-0000-4000-8000-%012d", st.handles)
		status, resp, async := st.exec(body.Statement, handle, body.Warehouse)
		if async && status == http.StatusOK {
			st.pending[handle] = resp
			writeJSON(w, http.StatusAccepted, map[string]any{
				"code": "333334", "message": "Asynchronous execution in progress. Use provided query id to perform query monitoring and management.",
				"statementHandle": handle, "statementStatusUrl": st.endpoint + "/" + handle})
			return true
		}
		writeJSON(w, status, resp)
		return true
	case req.Method == http.MethodGet && strings.HasPrefix(path, st.endpoint+"/"):
		s.Calls["GET "+st.endpoint+"/{statementHandle}"]++
		handle := strings.TrimPrefix(path, st.endpoint+"/")
		resp, ok := st.pending[handle]
		if !ok {
			sqlFail(w, http.StatusNotFound, "000709", "Statement "+handle+" not found", handle)
			return true
		}
		writeJSON(w, http.StatusOK, resp)
		return true
	}
	return false
}

// warehouseName is a request's warehouse as an exact name (plain, or in
// double quotes as Display writes one that is not).
func warehouseName(w string) string {
	if parts := splitDisplay(w); len(parts) == 1 && strings.HasPrefix(w, `"`) {
		if v, err := sqlapi.Parse("identifier", w); err == nil {
			return v.(string)
		}
	}
	return w
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func sqlFail(w http.ResponseWriter, status int, code, msg, handle string) {
	writeJSON(w, status, sqlFailure(code, msg, handle))
}

func sqlFailure(code, msg, handle string) map[string]any {
	return map[string]any{"code": code, "message": msg, "sqlState": "42000", "statementHandle": handle}
}

func result(handle string, cols []string, rows [][]any) map[string]any {
	rowType := []any{}
	for _, c := range cols {
		rowType = append(rowType, map[string]any{"name": c, "type": "text", "nullable": true})
	}
	if rows == nil {
		rows = [][]any{}
	}
	return map[string]any{
		"resultSetMetaData": map[string]any{"numRows": len(rows), "format": "jsonv2", "rowType": rowType,
			"partitionInfo": []any{map[string]any{"rowCount": len(rows)}}},
		"data": rows, "code": "090001", "sqlState": "00000", "statementHandle": handle,
		"statementStatusUrl": "/api/v2/statements/" + handle, "message": "Statement executed successfully.",
	}
}

func done(handle string) map[string]any {
	return result(handle, []string{"status"}, [][]any{{"Statement executed successfully."}})
}

// exec runs one statement: its HTTP status and answer, and whether a
// create (which answers 202 first).
func (st *sqlState) exec(stmt, handle, warehouse string) (int, any, bool) {
	// Every template that makes the statement; the most specific (the
	// most literal text) wins: ALTER SHARE {@id} SET COMMENT = {comment}
	// over ALTER {object_type} {object_name} SET {key} = {value}.
	type hit struct {
		sk    *sqlKind
		op    string // create, alter, unset, delete, read
		field string // alter, unset: the field; read: its index
		read  int
		vals  map[string]any
		lit   int
	}
	var hits []hit
	try := func(sk *sqlKind, op, field string, read int, p *sqlapi.Pattern) {
		if p == nil {
			return
		}
		if v, ok := p.Match(sk.k.SQL, stmt); ok {
			if wf := sk.k.SQL.Warehouse; wf != "" && warehouse != "" && op != "read" && op != "delete" {
				v[wf] = warehouseName(warehouse) // kept: the warehouse it was made or changed in
			}
			hits = append(hits, hit{sk, op, field, read, v, p.Used(stmt)})
		}
	}
	for _, sk := range st.kinds {
		try(sk, "create", "", 0, sk.create)
		for _, f := range sortedKeysOf(sk.alter) {
			try(sk, "alter", f, 0, sk.alter[f])
		}
		for _, f := range sortedKeysOf(sk.unset) {
			try(sk, "unset", f, 0, sk.unset[f])
		}
		for _, f := range sortedKeysOf(sk.add) {
			try(sk, "add", f, 0, sk.add[f])
		}
		for _, f := range sortedKeysOf(sk.remove) {
			try(sk, "remove", f, 0, sk.remove[f])
		}
		try(sk, "delete", "", 0, sk.delete)
		for i, p := range sk.teardown {
			if p != nil {
				try(sk, "teardown", strconv.Itoa(i), 0, p)
			}
		}
		for i, p := range sk.reads {
			try(sk, "read", "", i, p)
		}
	}
	if len(hits) == 0 {
		return 422, sqlFailure("001003", "SQL compilation error:\nsyntax error: the fake SQL API knows no template that makes this statement", handle), false
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].lit > hits[j].lit })
	// (Settings set field by field need one only to read: POLICY_REFERENCES.)
	if warehouse == "" && hits[0].sk.k.SQL.Warehouse != "" && (!hits[0].sk.k.SQL.PerField() || hits[0].op == "read") {
		// A kind that names a warehouse runs a query (a materialized view's
		// CREATE): Snowflake refuses it in a session with none.
		return 422, sqlFailure("000606", "No active warehouse selected in the current session.  Select an active warehouse with the 'use warehouse' command.", handle), false
	}
	// Settings set field by field (the account's) hold what the statement
	// sets, as does any other kind whose statement it is too (ALTER ACCOUNT
	// SET PASSWORD POLICY ..., an AccountParameter's ALTER ACCOUNT SET
	// {key} = {value}): in Snowflake they are the same setting. The others
	// answer it as they would without them.
	if hits[0].op != "read" {
		var rest []hit
		status, resp, async, took := 0, any(nil), false, false
		for _, h := range hits {
			if h.lit == hits[0].lit && h.op != "read" && h.sk.k.SQL.PerField() {
				status, resp, async = st.write(h.sk, h.op, h.field, h.vals, stmt, handle)
				took = true
			} else {
				rest = append(rest, h)
			}
		}
		if took && len(rest) == 0 {
			return status, resp, async
		}
		if took {
			hits = rest
		}
	}
	// A write on what is there: the most specific template whose kind holds
	// the record it names (all integrations share ALTER SECURITY
	// INTEGRATION; ALTER DATABASE ... SET is a secondary database's alter
	// and an object parameter's set), else a create, else the most specific.
	if hits[0].op != "read" {
		for _, h := range hits {
			switch {
			case h.op == "create":
				return st.create(h.sk, h.vals, handle)
			case h.op == "read":
			case st.find(h.sk, h.vals) != nil:
				return st.write(h.sk, h.op, h.field, h.vals, stmt, handle)
			}
		}
		h := hits[0]
		return st.write(h.sk, h.op, h.field, h.vals, stmt, handle)
	}
	var top []hit
	for _, h := range hits {
		if h.lit == hits[0].lit {
			top = append(top, h)
		}
	}
	// A read: the rows of every kind it is a read of (integrations of all
	// types share SHOW and DESCRIBE).
	type part struct {
		cols []string
		rows [][]any
	}
	var parts []part
	var cols []string
	named := false
	for _, h := range top {
		if h.op != "read" {
			continue
		}
		rd := h.sk.k.SQL.Read[h.read]
		_, named = h.vals["@id"]
		c, r := st.rows(h.sk, rd, h.vals)
		if named && len(rd.ListColumns) > 0 && st.find(h.sk, h.vals) != nil {
			named = false // there, with no elements: no rows
		}
		if cols == nil {
			cols = c
		}
		if len(r) > 0 {
			parts = append(parts, part{c, r})
		}
	}
	// Kinds whose reads share a statement (SHOW REPLICATION GROUPS of a
	// primary and a secondary) may map other columns: rows of more than
	// one answer every column any of them has.
	var rows [][]any
	switch len(parts) {
	case 0:
	case 1:
		cols, rows = parts[0].cols, parts[0].rows
	default:
		all := map[string]bool{}
		for _, p := range parts {
			for _, c := range p.cols {
				all[c] = true
			}
		}
		cols = sortedKeysOf(all)
		for _, p := range parts {
			for _, r := range p.rows {
				row := make([]any, len(cols))
				for i, c := range cols {
					for j, pc := range p.cols {
						if pc == c {
							row[i] = r[j]
						}
					}
				}
				rows = append(rows, row)
			}
		}
	}
	if named && len(rows) == 0 {
		return 422, sqlFailure("002003", "SQL compilation error:\nObject does not exist or not authorized.", handle), false
	}
	return http.StatusOK, result(handle, cols, rows), false
}

// write runs an alter, an unset or a delete on the record it names.
func (st *sqlState) write(sk *sqlKind, op, field string, vals map[string]any, stmt, handle string) (int, any, bool) {
	r := st.find(sk, vals)
	switch op {
	case "delete":
		return st.drop(sk, r, stmt, handle)
	case "add", "remove":
		if refused, status, resp := st.busy(sk, r, handle); refused {
			return status, resp, false
		}
		defer st.transition(sk, r, false)
		return st.delta(sk, r, field, vals, op == "add", handle)
	case "teardown":
		if r == nil {
			return 422, sqlFailure("002003", "SQL compilation error:\nObject does not exist or not authorized.", handle), false
		}
		if refused, status, resp := st.busy(sk, r, handle); refused {
			return status, resp, false
		}
		st.transition(sk, r, true)
		return http.StatusOK, done(handle), false
	}
	if refused, status, resp := st.busy(sk, r, handle); refused {
		return status, resp, false
	}
	defer st.transition(sk, r, false)
	return st.change(sk, r, field, vals, op == "unset", handle)
}

// key is where a record is kept: an object by its name; a setting by what
// it is set on (the delete's fields); a grant by all it grants.
func (st *sqlState) key(sk *sqlKind, vals map[string]any, id []string) string {
	q := sk.k.SQL
	if q.Mode == descriptor.SQLObject {
		return sk.k.Kind + "|" + strings.Join(id, "\x00")
	}
	keep := map[string]any{}
	for _, f := range keyFields(q) {
		keep[f] = canonical(q.Fields[f].Type, vals[f])
	}
	b, _ := json.Marshal(keep)
	return sk.k.Kind + "|" + string(b)
}

// keyFields are the fields a setting or grant is kept by: those its delete
// names. A setting's delete may name a field its create does not (who
// ownership goes back to): it is kept by the fields both name.
func keyFields(q *descriptor.SQL) []string {
	del, _ := descriptor.ParseSQLTemplate(q.Delete)
	fields := del.Fields()
	if q.Mode != descriptor.SQLSet {
		return fields
	}
	cr, _ := descriptor.ParseSQLTemplate(q.Create)
	inCreate := map[string]bool{}
	for _, f := range cr.Fields() {
		inCreate[f] = true
	}
	var out []string
	for _, f := range fields {
		if inCreate[f] {
			out = append(out, f)
		}
	}
	return out
}

// canonical is a value as the fake compares it: keywords upper-cased.
func canonical(typ string, v any) any {
	if m, ok := v.(map[string]any); ok { // an object: its fields as written
		b, _ := json.Marshal(m)
		return string(b)
	}
	switch x := v.(type) {
	case string:
		if typ == "keyword" || typ == "keywords" {
			return strings.ToUpper(x)
		}
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = canonical(typ, e)
		}
		return out
	}
	return v
}

func idParts(sk *sqlKind, vals map[string]any) []string {
	text, ok := vals["@id"].(string)
	if !ok {
		// Named by its id fields instead (ALTER USER {user} ... TOKEN
		// {name}): each as the statement wrote it, an exact name.
		var parts []string
		for _, f := range sk.k.SQL.ID {
			s, ok := vals[f].(string)
			if !ok {
				return nil
			}
			parts = append(parts, s)
		}
		return parts
	}
	parts, err := sqlapi.ParseID(quoteAll(text), len(sk.k.SQL.ID))
	if err != nil {
		return nil
	}
	return parts
}

// quoteAll writes a name Parse displayed (plain parts bare) back with every
// part quoted.
func quoteAll(display string) string {
	var out []string
	for _, p := range splitDisplay(display) {
		if !strings.HasPrefix(p, `"`) {
			p = sqlapi.QuoteExact(p)
		}
		out = append(out, p)
	}
	return strings.Join(out, ".")
}

func splitDisplay(v string) []string {
	var out []string
	in := false
	start := 0
	for i := 0; i < len(v); i++ {
		switch v[i] {
		case '"':
			in = !in
		case '.':
			if !in {
				out = append(out, v[start:i])
				start = i + 1
			}
		}
	}
	return append(out, v[start:])
}

func (st *sqlState) find(sk *sqlKind, vals map[string]any) *sqlRecord {
	return st.records[st.key(sk, vals, idParts(sk, vals))]
}

func (st *sqlState) create(sk *sqlKind, vals map[string]any, handle string) (int, any, bool) {
	q := sk.k.SQL
	id := idParts(sk, vals)
	key := st.key(sk, vals, id)
	rec := &sqlRecord{kind: sk.k, id: id, vals: map[string]any{}}
	for f, v := range vals {
		if !descriptor.SQLSpecial[f] {
			rec.vals[f] = v
		}
	}
	for i, f := range q.ID {
		if i < len(id) {
			rec.vals[f] = id[i]
		}
	}
	// What Snowflake makes (an IAM user ARN, a key's fingerprint, a
	// token's secret): made up, as text.
	st.made++
	for f, fd := range q.Fields {
		if fd.Output {
			rec.vals[f] = fmt.Sprintf("example-%s-%d", strings.ReplaceAll(f, "_", "-"), st.made)
		}
	}
	if q.Settle != nil { // asynchronous: in flight until it is read
		rec.vals[q.Settle.Field] = busyStatus(q.Settle)
		rec.settleLeft, rec.settleTo = len(q.Read), q.Settle.Ready[0]
	}
	answer := done(handle)
	if len(q.CreateColumns) > 0 {
		var cols []string
		var row []any
		for _, f := range sortedKeysOf(q.CreateColumns) {
			cols = append(cols, q.CreateColumns[f])
			row = append(row, rec.vals[f])
		}
		answer = result(handle, cols, [][]any{row})
	}
	switch q.Mode {
	case descriptor.SQLObject:
		if _, taken := st.records[key]; taken {
			return 422, sqlFailure("002002", "SQL compilation error:\nObject '"+strings.Join(id, ".")+"' already exists.", handle), false
		}
	case descriptor.SQLSet:
		if old := st.records[key]; old != nil {
			for f, v := range rec.vals {
				if !q.Fields[f].Output {
					old.vals[f] = v
				}
			}
			return http.StatusOK, answer, true
		}
	}
	st.records[key] = rec
	return http.StatusOK, answer, true
}

func (st *sqlState) change(sk *sqlKind, r *sqlRecord, field string, vals map[string]any, unset bool, handle string) (int, any, bool) {
	if r == nil {
		return 422, sqlFailure("002003", "SQL compilation error:\nObject does not exist or not authorized.", handle), false
	}
	if unset {
		delete(r.vals, field)
		return http.StatusOK, done(handle), false
	}
	for f, v := range vals { // every field the statement sets (FREQUENCY with START_TIMESTAMP)
		if !descriptor.SQLSpecial[f] && v != nil {
			r.vals[f] = v
		}
	}
	return http.StatusOK, done(handle), false
}

// delta adds a list field's elements to the record, or removes them.
func (st *sqlState) delta(sk *sqlKind, r *sqlRecord, field string, vals map[string]any, add bool, handle string) (int, any, bool) {
	if r == nil {
		return 422, sqlFailure("002003", "SQL compilation error:\nObject does not exist or not authorized.", handle), false
	}
	typ := sk.k.SQL.Fields[field].Type
	have, _ := r.vals[field].([]any)
	elems, _ := vals[field].([]any)
	in := func(list []any, e any) bool {
		for _, x := range list {
			if fmt.Sprint(canonical(typ, x)) == fmt.Sprint(canonical(typ, e)) {
				return true
			}
		}
		return false
	}
	out := []any{}
	if add {
		out = append(out, have...)
		for _, e := range elems {
			if !in(out, e) {
				out = append(out, e)
			}
		}
	} else {
		for _, e := range have {
			if !in(elems, e) {
				out = append(out, e)
			}
		}
	}
	r.vals[field] = out
	return http.StatusOK, done(handle), false
}

func (st *sqlState) drop(sk *sqlKind, r *sqlRecord, stmt, handle string) (int, any, bool) {
	if r == nil {
		if strings.Contains(stmt, " IF EXISTS ") || sk.k.SQL.Mode != descriptor.SQLObject {
			return http.StatusOK, done(handle), false
		}
		return 422, sqlFailure("002003", "SQL compilation error:\nObject does not exist or not authorized.", handle), false
	}
	if set := sk.k.SQL.Settle; set != nil {
		// Snowflake drops an asynchronous object only once it is torn down.
		if status := fmt.Sprint(r.vals[set.Field]); r.settleLeft > 0 || (len(set.Gone) > 0 && !descriptor.SettleIn(set.Gone, status)) {
			return 422, sqlFailure("000603", "SQL execution error: the object is "+status+"; it can be dropped only once it is "+strings.Join(set.Gone, " or ")+" (the fake SQL API)", handle), false
		}
	}
	for key, rec := range st.records {
		if rec == r {
			delete(st.records, key)
		}
	}
	return http.StatusOK, done(handle), false
}

// busyStatus is the status the fake shows while an asynchronous object's
// work is in flight.
func busyStatus(set *descriptor.SQLSettle) string {
	if len(set.Busy) > 0 {
		return set.Busy[0]
	}
	return "IN_PROGRESS"
}

// busy refuses a statement on an asynchronous object whose work is still
// in flight, as Snowflake does.
func (st *sqlState) busy(sk *sqlKind, r *sqlRecord, handle string) (bool, int, any) {
	if r == nil || sk.k.SQL.Settle == nil || r.settleLeft == 0 {
		return false, 0, nil
	}
	status := fmt.Sprint(r.vals[sk.k.SQL.Settle.Field])
	return true, 422, sqlFailure("000603", "SQL execution error: the object is "+status+"; Snowflake refuses this statement until it settles (the fake SQL API)", handle)
}

// transition puts an asynchronous object in flight after a statement on
// it: a change settles where it was (or Ready), a teardown in Gone.
func (st *sqlState) transition(sk *sqlKind, r *sqlRecord, teardown bool) {
	set := sk.k.SQL.Settle
	if r == nil || set == nil {
		return
	}
	to, via := fmt.Sprint(r.vals[set.Field]), busyStatus(set)
	if descriptor.SettleIn(set.Busy, "UPDATING") { // a change: updating, where the kind has the status
		via = "UPDATING"
	}
	if !descriptor.SettleIn(set.Ready, to) {
		to = set.Ready[0]
	}
	if teardown {
		to = ""
		if len(set.Gone) > 0 {
			to = set.Gone[0]
		}
		for _, step := range set.Teardown {
			if step.Run == "" && len(step.When) > 0 { // the status it waits in
				via = step.When[0]
				break
			}
		}
	}
	r.vals[set.Field] = via
	r.settleLeft, r.settleTo = len(sk.k.SQL.Read), to
}

// rows answers one read of a kind: the rows of its records the statement
// names (by name, by the name's parent, by a LIKE pattern, by field).
func (st *sqlState) rows(sk *sqlKind, rd descriptor.SQLRead, vals map[string]any) ([]string, [][]any) {
	q := sk.k.SQL
	var recs []*sqlRecord
	var keys []string
	for key := range st.records {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		r := st.records[key]
		if r.kind == sk.k && selects(q, r, vals) {
			recs = append(recs, r)
		}
	}
	// An asynchronous object's work goes on as it is read: once read
	// settleLeft times, it is done.
	defer func() {
		for _, r := range recs {
			if q.Settle != nil && r.settleLeft > 0 {
				if r.settleLeft--; r.settleLeft == 0 {
					r.vals[q.Settle.Field] = r.settleTo
				}
			}
		}
	}()
	if rd.Properties != nil {
		if parts := strings.Split(rd.Properties.Value, "."); len(parts) > 1 {
			// The value is a name answered in parts (POLICY_REFERENCES'
			// POLICY_DB, POLICY_SCHEMA, POLICY_NAME).
			cols := append([]string{rd.Properties.Name}, parts...)
			var rows [][]any
			for _, r := range recs {
				for _, f := range sortedKeysOf(rd.Columns) {
					if v, ok := r.vals[f]; ok {
						if names := parseDisplay(column(q.Fields[f].Type, v)); len(names) == len(parts) {
							row := []any{rd.Columns[f]}
							for _, n := range names {
								row = append(row, n)
							}
							rows = append(rows, row)
						}
					}
				}
			}
			return cols, rows
		}
		cols := []string{rd.Properties.Name, "property_type", rd.Properties.Value, "property_default"}
		var rows [][]any
		for _, r := range recs {
			for _, f := range sortedKeysOf(rd.Columns) {
				if v, ok := r.vals[f]; ok {
					rows = append(rows, []any{rd.Columns[f], "String", column(q.Fields[f].Type, v), ""})
				} else if rd.Properties.Null != "" { // DESCRIBE USER answers every property
					rows = append(rows, []any{rd.Columns[f], "String", rd.Properties.Null, ""})
				}
			}
		}
		return cols, rows
	}
	if len(rd.ListColumns) > 0 && len(rd.Match) == 0 { // a row per element
		f := sortedKeysOf(rd.ListColumns)[0]
		col := rd.ListColumns[f]
		var rows [][]any
		for _, r := range recs {
			elems, _ := r.vals[f].([]any)
			for _, e := range elems {
				rows = append(rows, []any{column(descriptor.SQLListElem(q.Fields[f].Type), e)})
			}
		}
		return []string{col}, rows
	}
	// A match on joined columns (POLICY_DB.POLICY_SCHEMA.POLICY_NAME)
	// answers its parts as a mapped name does.
	mapped := map[string]string{}
	for f, c := range rd.Columns {
		mapped[f] = c
	}
	colSet := map[string]bool{}
	for c, m := range rd.Match {
		if strings.Contains(c, ".") {
			if f, ok := fieldOf(m); ok {
				mapped[f] = c
			}
			continue
		}
		colSet[c] = true
	}
	for _, c := range mapped {
		for _, sub := range strings.Split(c, ".") {
			colSet[sub] = true
		}
	}
	cols := sortedKeysOf(colSet)
	var rows [][]any
	for _, r := range recs {
		var list string
		for c, m := range rd.Match {
			if f, ok := strings.CutPrefix(m, "{"); ok {
				if f = strings.TrimSuffix(f, "}"); descriptor.SQLListElem(q.Fields[f].Type) != "" {
					list = c
				}
			}
		}
		n := 1
		var elems []any
		if list != "" {
			elems, _ = r.vals[strings.Trim(rd.Match[list], "{}")].([]any)
			n = len(elems)
		}
		for i := 0; i < n; i++ {
			row := make([]any, len(cols))
			for j, c := range cols {
				if m, ok := rd.Match[c]; ok {
					f, isField := strings.CutPrefix(m, "{")
					f = strings.TrimSuffix(f, "}")
					switch {
					case !isField:
						row[j] = m
					case c == list:
						row[j] = column(descriptor.SQLListElem(q.Fields[f].Type), elems[i])
					default:
						row[j] = column(q.Fields[f].Type, r.vals[f])
					}
					continue
				}
				for f, col := range mapped {
					subs := strings.Split(col, ".")
					for n, sub := range subs {
						switch {
						case sub != c:
						case len(subs) == 1:
							row[j] = column(q.Fields[f].Type, r.vals[f])
						default: // one part of a name
							if parts := parseDisplay(r.vals[f]); len(parts) == len(subs) {
								row[j] = parts[n]
							}
						}
					}
				}
			}
			rows = append(rows, row)
		}
	}
	return cols, rows
}

func fieldOf(m string) (string, bool) {
	if len(m) > 2 && m[0] == '{' && m[len(m)-1] == '}' {
		return m[1 : len(m)-1], true
	}
	return "", false
}

// selects: the record is one the read's statement names.
func selects(q *descriptor.SQL, r *sqlRecord, vals map[string]any) bool {
	for f, v := range vals {
		switch f {
		case "@id":
			if strings.Join(parseDisplay(v), "\x00") != strings.Join(r.id, "\x00") {
				return false
			}
		case "@parent":
			p := parseDisplay(v)
			if len(r.id) != len(p)+1 || strings.Join(p, "\x00") != strings.Join(r.id[:len(p)], "\x00") {
				return false
			}
		case "@like":
			if len(r.id) == 0 || !like(v.(string), r.id[len(r.id)-1]) {
				return false
			}
		default:
			typ := q.Fields[f].Type
			if fmt.Sprint(canonical(typ, r.vals[f])) != fmt.Sprint(canonical(typ, v)) {
				return false
			}
		}
	}
	return true
}

func parseDisplay(v any) []string {
	s, _ := v.(string)
	parts, err := sqlapi.ParseID(quoteAll(s), len(splitDisplay(s)))
	if err != nil {
		return nil
	}
	return parts
}

// like is SQL's LIKE, ignoring case as SHOW does: _ one character, % any.
func like(pattern, s string) bool {
	var b strings.Builder
	b.WriteString(`(?is)^`)
	for _, r := range pattern {
		switch r {
		case '_':
			b.WriteString(`.`)
		case '%':
			b.WriteString(`.*`)
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString(`$`)
	ok, _ := regexp.MatchString(b.String(), s)
	return ok
}

// column is a value as a SHOW or DESCRIBE answers it: text, or null.
func column(typ string, v any) any {
	if v == nil {
		return nil
	}
	if elem := descriptor.SQLListElem(typ); elem != "" {
		list, _ := v.([]any)
		parts := make([]string, 0, len(list))
		for _, e := range list {
			parts = append(parts, fmt.Sprint(column(elem, e)))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	switch x := v.(type) {
	case bool:
		return strconv.FormatBool(x)
	case float64:
		return descriptor.Text(x)
	case map[string]any: // an argument
		return fmt.Sprintf("%v %v", x["name"], x["type"])
	}
	return fmt.Sprint(v)
}

func sortedKeysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
