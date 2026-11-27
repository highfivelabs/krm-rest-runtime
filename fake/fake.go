// SPDX-License-Identifier: Apache-2.0

// Package fake is an in-memory API built from the same descriptors as the
// controllers: every kind's create, read, update and delete paths answer as
// the descriptor says the real API does. It proves the controllers and the
// descriptors agree with each other; it proves nothing about the real API.
package fake

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/highfivelabs/krm-rest-runtime/descriptor"
	"github.com/highfivelabs/krm-rest-runtime/rest"
)

type role int

const (
	collection role = iota
	item
	list        // a read that lists (Read.Find)
	assocAdd    // an assoc create
	assocList   // an assoc read
	assocDel    // an assoc undo
	replace     // a create that replaces the resource in place (Kind.Replace)
	prep        // a call the item needs before it can be deleted (Kind.BeforeDelete)
	adoptList   // the list an adopting kind finds its resource in (Kind.Adopt)
	removal     // a delete that is not a DELETE (a POST of a fixed body), or that names the item elsewhere
	moved       // an update on another path than the item's, the ID in its body
	setList     // a set kind's read: every element under its path
	setAdd      // a set kind's create: one element more
	setElem     // a set kind's update or delete of one element
	stateCall   // a call that puts the resource in a state (Kind.State)
	setHolder   // a call on the resource a set kind's elements are in (Set.Holder)
	madeList    // the list a create that answers no ID finds its resource in (ID.Find)
	stateBefore // a call made before a state's call (State.Before): answered, nothing kept
	setBatch    // a set kind's batch list's add or clear (Set.Batch)
	unreadWrite // the write of a kind that is never read (Kind.Unread)
)

type route struct {
	method string
	path   string
	re     *regexp.Regexp
	kind   *descriptor.Kind
	role   role
	// query must all be present in the request for the route to match.
	query map[string]string
	// prep is the index of a prep route's call in Kind.BeforeDelete.
	prep int
	// op is the call the route answers.
	op *descriptor.Op
	// state is the value a stateCall route puts the resource in.
	state string
}

// Server is the fake API.
type Server struct {
	mu     sync.Mutex
	routes []route
	store  map[string]map[string]any
	// owner is, by stored path, the kind that made it and the path
	// parameters it was made with: a find read lists its kind's objects
	// whose parameters agree with the list's (GitHub's role teams are
	// written at .../teams/{team_slug}/{role_id}, listed at .../{role_id}/teams).
	owner  map[string]owned
	nextID int
	// prepared holds, by item path, which BeforeDelete calls were made: a
	// kind that has them answers 403 to a DELETE until every one was.
	prepared map[string]map[int]bool
	// Calls counts requests by "METHOD path-template" ("GRAPHQL
	// operation" for a GraphQL one), for assertions.
	Calls map[string]int
	// gql holds GraphQL kinds' operations by name; gqlPaths their
	// endpoints (graphql.go).
	gql      map[string]gqlRoute
	gqlPaths map[string]bool
	// deleted is, by path, the CRUD kind whose item a DELETE removed there:
	// a read of it finds none, though another kind shares the path (pick).
	deleted map[string]*descriptor.Kind
	// sql is the SQL API the sql kinds' statements go to (sql.go).
	sql *sqlState
	// seeds are the resources Element kinds' samples go in, made the
	// first time they are asked for (element.go).
	seeds map[string]*descriptor.Kind
	// riders are settings kinds read on another kind's item path, which
	// act on that kind's resource (seedHolders).
	riders map[*descriptor.Kind]bool
	// batches holds set kinds' batch lists (Set.Batch), by their list's
	// path: each element's key and the value the API answers for it.
	batches map[string]map[string]any
}

// New builds a fake for every kind in f.
type owned struct {
	kind   *descriptor.Kind
	params map[string]string
}

func New(f *descriptor.File) *Server {
	f = f.Reconcilable() // a draft's calls are guesses: no routes for them
	s := &Server{owner: map[string]owned{}, store: map[string]map[string]any{}, nextID: 270000000, Calls: map[string]int{}, prepared: map[string]map[int]bool{}, deleted: map[string]*descriptor.Kind{},
		gql: map[string]gqlRoute{}, gqlPaths: map[string]bool{}, batches: map[string]map[string]any{}}
	s.sql = newSQLState(f)
	s.seedHolders(f)
	for i := range f.Kinds {
		k := &f.Kinds[i]
		if k.SQL != nil {
			continue // its statements go to the SQL API (sql.go)
		}
		if k.IsGraphQL() {
			s.addGraphQL(k)
			if !k.IsMixed() {
				continue
			}
			// A mixed kind's REST calls act on the object its GraphQL
			// ones do (serveMixed).
			for _, op := range []*descriptor.Op{k.Create, &k.Read, k.Update, k.Delete} {
				if op != nil && op.GraphQL == nil {
					r := item
					if op == k.Create {
						r = collection
					}
					s.routes = append(s.routes, route{method: op.Method, path: op.Path, re: compile(op.Path), kind: k, role: r, query: op.Query, op: op})
				}
			}
			continue
		}
		add := func(op *descriptor.Op, r role) {
			if op != nil {
				s.routes = append(s.routes, route{method: op.Method, path: op.Path, re: compile(op.Path), kind: k, role: r, query: op.Query, op: op})
			}
		}
		switch {
		case k.Unread != "":
			add(k.Update, unreadWrite) // nothing else: it is never read
			continue
		case k.Shape == descriptor.Assoc:
			add(k.Create, assocAdd)
			add(&k.Read, assocList)
			add(k.Delete, assocDel)
			continue
		case k.Shape == descriptor.SetShape:
			add(&k.Read, setList)
			add(k.Create, setAdd)
			add(k.Update, setElem)
			add(k.Delete, setElem)
			add(k.Set.ReadEach, setElem)
			if h := k.Set.Holder; h != nil {
				add(&h.Create, setHolder)
				add(&h.Read, setHolder)
				add(&h.Delete, setHolder)
			}
			if b := k.Set.Batch; b != nil {
				add(&b.Add, setBatch)
				add(&b.Clear, setBatch)
			}
			continue
		case k.Shape == descriptor.CRUD:
			add(k.Create, collection)
		case k.Shape == descriptor.Upsert && k.Create != nil:
			add(k.Create, item)
		}
		if k.Replace != nil {
			s.routes = append(s.routes, route{method: k.Replace.Method, re: compile(k.Replace.Path), kind: k, role: replace, query: k.Replace.Query, op: &k.Replace.Op})
		}
		if k.Read.Find != nil {
			add(&k.Read, list)
		} else {
			add(&k.Read, item)
		}
		if k.Adopt != nil {
			add(&k.Adopt.Op, adoptList)
		}
		if k.ID != nil && k.ID.Find != nil {
			add(&k.ID.Find.Op, madeList)
		}
		add(k.Update, item)
		if k.Element == nil { // an Element kind's delete is its write
			add(k.Delete, item)
		}
		s.addStateRoutes(k)
		for j := range k.BeforeDelete {
			op := &k.BeforeDelete[j]
			s.routes = append(s.routes, route{method: op.Method, re: compile(op.Path), kind: k, role: prep, query: op.Query, prep: j, op: op})
		}
	}
	// Literal segments win over placeholders: /waiting_rooms/settings is not
	// /waiting_rooms/{id}. A route that needs query parameters wins over one
	// that does not (createMode=orReplace over a plain create).
	sort.SliceStable(s.routes, func(i, j int) bool {
		a, b := s.routes[i], s.routes[j]
		if ca, cb := catchAll(a.re), catchAll(b.re); ca != cb {
			return cb // a path of only a {+param} (Vault's generic endpoint) answers what no other route does
		}
		if a.re.NumSubexp() != b.re.NumSubexp() {
			return a.re.NumSubexp() < b.re.NumSubexp()
		}
		return len(a.query) > len(b.query)
	})
	return s
}

// sameCreate reports whether two kinds are made by the same call
// (PagerDuty's business and technical service dependencies, both by POST
// /service_dependencies/associate): the fake cannot tell which made an
// object, so either finds it.
func sameCreate(a, b *descriptor.Kind) bool {
	return a.Create != nil && b.Create != nil && a.Create.Method == b.Create.Method && a.Create.Path == b.Create.Path && a.Create.GraphQL == nil
}

// catchAll reports whether a route's pattern is a single {+param} and
// nothing else: any path matches it.
func catchAll(re *regexp.Regexp) bool { return re.String() == "^/(.+)$" }

func compile(tmpl string) *regexp.Regexp {
	phs := regexp.MustCompile(`\{[^}]+\}`).FindAllString(tmpl, -1)
	parts := regexp.MustCompile(`\{[^}]+\}`).Split(tmpl, -1)
	var b strings.Builder
	b.WriteString("^")
	for i := range parts {
		b.WriteString(regexp.QuoteMeta(parts[i]))
		if i < len(phs) {
			if strings.HasPrefix(phs[i], "{+") {
				b.WriteString("(.+)") // a value that keeps its slashes
			} else {
				b.WriteString("([^/]+)")
			}
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// Get returns a stored object by its concrete path (for assertions).
func (s *Server) Get(path string) (map[string]any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.store[path]
	return v, ok
}

// Len is how many objects the fake holds.
func (s *Server) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.store)
}

// Paths lists stored object paths.
func (s *Server) Paths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for p := range s.store {
		out = append(out, p)
	}
	return out
}

func (s *Server) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.serveSQL(w, req) {
		return
	}
	path := req.URL.EscapedPath()
	s.seed(path)
	if req.Method == http.MethodPost && s.gqlPaths[path] {
		s.serveGraphQL(w, req)
		return
	}
	var rt *route
	var also, alt []*route
	for i := range s.routes {
		r := &s.routes[i]
		if r.method == req.Method && r.re.MatchString(path) && queryMatches(r.query, req.URL.Query()) {
			if rt == nil {
				rt = r
			} else if r.path != "" && r.path == rt.path && r.kind != rt.kind {
				// The same template, not only the same pattern: Vault's
				// /{gcpkms_mount_path}/config and /{kv_mount_path}/config are
				// told apart by their mount, as the first route that matches.
				also = append(also, r) // two kinds on one URL (a group, and its file template setting)
			} else if r.re.String() == rt.re.String() && r.kind != rt.kind {
				alt = append(alt, r)
			}
		}
	}
	if rt != nil && rt.kind.Shape == descriptor.CRUD && rt.role == item && len(alt) > 0 {
		// The first route is a CRUD item's, under another name: the
		// kinds on the same pattern answer too, as on one template
		// (GitLab's /users/{user_id}, a service account, and /users/{id},
		// a user, where a user's identity and avatar are written too).
		also = append(also, alt...)
	}
	shared := len(also) > 0
	listed := map[*descriptor.Kind]bool{} // kinds whose finds read this list (Cloudflare's DLP entries)
	var marks []*descriptor.Kind          // kinds whose absent marker this read answers (marksShared)
	if shared {
		cands := append([]*route{rt}, also...)
		rt = s.pick(cands, path)
		for _, r := range cands {
			if r.role == list {
				listed[r.kind] = true
			}
			if r.kind != rt.kind && r.op == &r.kind.Read && marksShared(r.kind) {
				marks = append(marks, r.kind)
			}
		}
	}
	if rt == nil {
		http.Error(w, `{"message":"no route"}`, http.StatusNotFound)
		return
	}
	rt = s.undoRoute(rt, req)
	s.Calls[CallKey(req.Method, rt.re.String(), rt.query)]++
	k := rt.kind
	var body map[string]any
	var raw any // the request's JSON body, as sent
	if ct := req.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/form-data") {
		var err error
		body, err = decodeMultipart(req, ct, k)
		if err != nil {
			http.Error(w, `{"message":"bad multipart"}`, http.StatusBadRequest)
			return
		}
	} else if req.Body != nil {
		data, _ := io.ReadAll(req.Body)
		if len(data) > 0 {
			var v any
			if err := json.Unmarshal(data, &v); err != nil {
				http.Error(w, `{"message":"bad json"}`, http.StatusBadRequest)
				return
			}
			raw = v
			env := k.BodyEnvelope
			if rt.op != nil && rt.op.BodyEnvelope != "" {
				env = rt.op.BodyEnvelope // an assoc's undo: {"remove": [...]}
			}
			if rt.op != nil && rt.op.BareBody {
				env = "" // sent as it is (an RPC delete's {"id": ...})
			}
			v = descriptor.Dig(v, env)
			if k.BodyArray || rt.op != nil && rt.op.BodyArray {
				if arr, ok := v.([]any); ok && len(arr) > 0 {
					v = arr[0]
				}
			}
			if k.BodyField != "" {
				if arr, ok := v.([]any); ok {
					body = map[string]any{k.BodyField: arr}
				}
			} else {
				body, _ = v.(map[string]any)
			}
		}
	}
	if rt.op != nil && rt.op.Request != nil && raw != nil {
		// Built by the call from the spec (Datadog's datastore item, each
		// call another shape): the spec's fields, where the call put them.
		body = descriptor.RequestValues(rt.op.Request, raw)
		for _, p := range k.Params {
			if !p.InBody {
				delete(body, p.Field) // named by the call (an item_key), not kept
			}
		}
	}
	if k.IsMixed() {
		s.serveMixed(w, rt, req.Method, path, body)
		return
	}
	if k.ReadsByBody() {
		if shared {
			rt = s.pickByBody(append([]*route{rt}, also...), path, body, raw)
		}
		s.serveByBody(w, rt, path, body, raw)
		return
	}
	rt, path = s.retarget(rt, req.Method, path, body)
	if msg := s.check(rt, req, path, raw); msg != "" {
		http.Error(w, `{"message":"`+msg+`"}`, http.StatusBadRequest)
		return
	}
	if o := k.Order; o != nil && rt.op == k.Update {
		// A reorder: kept as the order, where the list is read, with the
		// call's other fields (Datadog's data.id), as the API keeps them.
		if p, ok := samePlaceholders(rt.re, k.Update.Path, path, k.Read.Path); ok {
			path = p
		}
		mb, _ := raw.(map[string]any)
		body = descriptor.MergeAt(mb, o.At, nil)
		if !strings.Contains(o.At, "/") {
			delete(body, o.At)
		}
		body[o.Field] = o.Sent(raw)
	}
	switch {
	case rt.role == setBatch:
		s.serveBatch(w, rt, path, body)
	case rt.role == unreadWrite:
		// Kept whole where it is written, and answered as sent.
		s.store[path] = clone(body)
		s.reply(w, http.StatusOK, k, body)
	case rt.role == setList || rt.role == setAdd || rt.role == setElem || rt.role == setHolder:
		s.serveSet(w, rt, req.Method, path, req.URL.Query(), body)
	case elementOwnsPath(rt):
		s.serveElement(w, rt, req.Method, path, body)
	case rt.role == stateCall:
		s.serveState(w, rt, path)
	case rt.role == stateBefore:
		writeJSON(w, http.StatusOK, map[string]any{})
	case rt.role == removal:
		s.remove(w, rt, path, body)
	case rt.role == list && k.Read.Find.Key != "":
		s.keyedList(w, rt, path)
	case rt.role == list:
		want := routeParams(rt.re, rt.path, path)
		var out []any
		for key, obj := range s.store {
			o, ok := s.owner[key]
			if !ok || (o.kind != k && !listed[o.kind] && !sameCreate(o.kind, k)) {
				continue
			}
			match := true
			for n, v := range want {
				if got, has := o.params[n]; has && got != v {
					match = false
				}
			}
			if match && k.Read.Find.Field == "" && o.kind == k {
				// Bare names (Vault's LIST: {"keys": ["a", "b"]}).
				out = append(out, o.params[k.Read.Find.Param])
			} else if match && o.kind == k && k.Read.Envelope != "" {
				// The object is inside each element (Datadog's managed
				// organizations: {"attributes": {...}}).
				out = append(out, descriptor.Wrap(k.Read.Envelope, withServerFields(k, obj)))
			} else if match {
				out = append(out, obj)
			}
		}
		if writtenWhereListed(k) && rt.op == &k.Read {
			// Written where it is listed, by a write of that item (a GitLab
			// user's identity, PUT /users/{id} with provider and
			// extern_uid; a team's one EMU group mapping): the item is the
			// list's element, whichever kind's route wrote it.
			out = nil
			if obj, ok := s.store[path]; ok && (k.Read.Find.Field == "" || descriptor.Dig(obj, strings.ReplaceAll(k.Read.Find.Field, ".", "/")) != nil) {
				out = []any{obj}
			}
		}
		sortByKey(out)
		if lists := s.listsOn(cands(rt, also), listed, path); lists != nil {
			// Kinds that each find their own list in one answer (a GitLab
			// runner controller's instance_level_scopings and
			// runner_level_scopings): each list holds its kind's.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(lists)
			return
		}
		if o, ok := s.owner[path]; ok && o.kind != k {
			// The list is inside another kind's item (Atlas's service account
			// holds its secrets): that item, with this kind's list in it.
			obj := clone(s.store[path])
			if len(out) > 0 {
				setPath(obj, k.ListAt(), out)
			} else if at := strings.ReplaceAll(k.ListAt(), ".", "/"); !writtenWhereListed(k) && descriptor.Dig(withServerFields(o.kind, obj), at) == nil {
				// None: the item as its kind answers it, with an empty list
				// where it has none of its own. (One written where it is
				// listed is in the item itself: the item as its kind made it.)
				setPath(obj, at, []any{})
			}
			s.reply(w, http.StatusOK, o.kind, obj)
			return
		}
		if shared && len(out) == 0 {
			for _, r := range append([]*route{rt}, also...) {
				if r.kind.Shape == descriptor.Settings && r.method == http.MethodGet {
					s.reply(w, http.StatusOK, r.kind, map[string]any{}) // a settings kind's item: there before anything is written
					return
				}
			}
			lists := true // only kinds that find their objects in this list (two kinds of token of one service account): empty
			for _, r := range append([]*route{rt}, also...) {
				lists = lists && r.role == list
			}
			if !lists {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound) // nor is the item it is in
				return
			}
		}
		s.replyList(w, k.ListAt(), out)
	case rt.role == assocAdd:
		s.store[AssocKey(k, body)] = clone(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	case rt.role == assocList:
		var out []any
		for key, obj := range s.store {
			if strings.HasPrefix(key, "assoc:"+k.Kind+":") {
				out = append(out, obj)
			}
		}
		sortByKey(out)
		s.replyList(w, k.ListAt(), out)
	case rt.role == assocDel && body == nil && hasTemplates(rt.op.Query):
		// Undone by a query naming the link's fields (Kafka's DELETE .../acls?principal=...).
		for key, obj := range s.store {
			if strings.HasPrefix(key, "assoc:"+k.Kind+":") && linkMatches(rt.op.Query, req.URL.Query(), obj) {
				delete(s.store, key)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	case rt.role == assocDel:
		delete(s.store, AssocKey(k, body))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	case rt.role == replace:
		s.replace(w, k, path, body, routeParams(rt.re, k.Replace.Path, path))
	case rt.role == prep:
		s.prepare(w, rt, path)
	case rt.role == adoptList:
		s.adoptList(w, rt, path)
	case rt.role == madeList:
		// What the kind's creates made, as the list answers them.
		want := routeParams(rt.re, rt.path, path)
		var out []any
		for key, obj := range s.store {
			if o, ok := s.owner[key]; ok && o.kind == k && agree(o.params, want) {
				out = append(out, obj)
			}
		}
		sortByKey(out)
		at := ""
		if k.ID.Find.Find != nil {
			at = k.ID.Find.Find.List
		}
		s.replyList(w, at, out)
	case rt.role == collection: // CRUD create
		obj := clone(body)
		// A key the caller chose (a label's name, a variable's key) stays;
		// otherwise the server assigns a number.
		var id any
		if k.ID.IsTemplate() {
			id, _ = k.ID.Compose(obj)
		} else {
			id = descriptor.Dig(obj, k.ID.From)
		}
		if id == nil || id == "" {
			id = obj[k.ID.Param]
			if id != nil && id != "" && !k.ID.IsTemplate() && descriptor.Dig(obj, k.ID.From) == nil {
				// The caller named it in the body (a member's user_id): the API
				// answers it as the ID field too, as GitLab's membership does.
				setPath(obj, k.ID.From, id)
			}
		}
		if id == nil || id == "" {
			s.nextID++
			id = s.nextID
			setPath(obj, k.ID.From, id)
		}
		if fd := k.Read.Find; fd != nil && fd.Field != "" && fd.Param == k.ID.Param {
			// Listed under another name than the create answers it by
			// (Datadog's EventBridge source: made as event_source_name,
			// listed as name), inside the element's envelope where the
			// read has one (a managed organization's attributes.public_id).
			at := strings.ReplaceAll(fd.Field, ".", "/")
			if env := k.Read.Envelope; env != "" && strings.HasPrefix(at, env+"/") {
				at = strings.TrimPrefix(at, env+"/")
			}
			if descriptor.Dig(obj, at) == nil {
				setPath(obj, at, id)
			}
		}
		key := path + "/" + url.PathEscape(idText(id))
		s.fillCarried(k, obj)
		if k.Read.Find == nil {
			// Store the item where its own read path says it lives: that differs from
			// "create path + id" when the path ends in a slash (Sentry) or the item is
			// made by a POST elsewhere (a team under /organizations/{org}/teams/).
			if p, ok := itemPath(k, rt.re, path, idText(id), obj); ok {
				key = p
			}
		} else if p, ok := findItemPath(k, rt, path, idText(id), obj); ok {
			// Read from a list, written on its own path (Cloudflare's audio track,
			// made by POST .../audio/copy, lives at .../audio/{audio_identifier}).
			key = p
		}
		s.store[key] = obj
		made := routeParams(rt.re, rt.path, path)
		if fd := k.Read.Find; fd != nil && fd.Field == "" {
			made[fd.Param] = idText(id) // listed by its bare name (Vault's LIST of EAB IDs)
		}
		s.owner[key] = owned{k, made}
		s.replyOp(w, http.StatusCreated, k, rt.op, obj)
	case req.Method == http.MethodGet:
		obj, ok := s.store[path]
		if !ok && k.ReadsElsewhere() {
			// Read on another path than it is kept at (Grafana's public
			// dashboard, read at .../public-dashboards): the kind's object
			// whose path parameters agree with this one's.
			obj, ok = s.ownedBy(k, routeParams(rt.re, rt.path, path))
		}
		if !ok && len(k.Read.Absent) > 0 {
			// 200 all the same, saying there is none (Atlas: state UNCONFIGURED).
			var resp any = map[string]any{}
			for f, v := range k.Read.Absent {
				if v != nil {
					setPath(resp.(map[string]any), strings.ReplaceAll(f, ".", "/"), v)
				}
			}
			if k.Envelope != "" { // none of the fields the API sets on an object that is there
				parts := strings.Split(k.Envelope, "/")
				for i := len(parts) - 1; i >= 0; i-- {
					resp = map[string]any{parts[i]: resp}
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		if !ok {
			if k.Shape != descriptor.Settings {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			obj = map[string]any{}
		}
		if len(marks) > 0 {
			obj = withMarks(marks, obj)
		}
		if rt.op != nil && rt.op.Raw != "" {
			// Not JSON: the field's value as it is (Workers KV).
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte(idText(obj[rt.op.Raw])))
			return
		}
		s.replyOp(w, http.StatusOK, k, rt.op, obj)
	case req.Method == http.MethodDelete:
		if k.Delete != nil && k.Read.Find == nil && k.Delete.Path != k.ItemPath() {
			// Deleted on another path than it is read from (Grafana's
			// DELETE /admin/users/{id} for GET /users/{id}).
			if p, ok := samePlaceholders(rt.re, k.Delete.Path, path, k.ItemPath()); ok {
				path = p
			} else if p, ok := s.namedByCurrent(k, routeParams(rt.re, rt.path, path)); ok {
				path = p
			}
		} else if _, ok := s.store[path]; !ok && k.Delete != nil && k.Read.Find != nil && k.Delete.Path != k.Read.Path {
			// Kept where it is listed, written there by a write of that
			// item (a GitLab user's identity, PUT /users/{id}), and deleted
			// on a path of its own (DELETE /users/{id}/identities/{provider}).
			if p, ok := samePlaceholders(rt.re, k.Delete.Path, path, k.Read.Path); ok {
				o, own := s.owner[p]
				if own && o.kind == k || writtenWhereListed(k) && s.holds(k, p, routeParams(rt.re, rt.path, path)) {
					path = p
				}
			}
		}
		if _, ok := s.store[path]; !ok {
			// Deleted on a path its create's parameters do not name
			// (Atlas's project API key, deleted as the organization's, on
			// the ApiKey kind's path): the object whose ID the path names,
			// of a kind whose delete this route is.
		found:
			for _, r := range append([]*route{rt}, also...) {
				rk := r.kind
				if rk.ID == nil || rk.Delete == nil || r.op != rk.Delete {
					continue
				}
				id := routeParams(r.re, r.path, path)[rk.ID.Param]
				for p, o := range s.owner {
					if id != "" && o.kind == rk && idText(descriptor.Dig(s.store[p], rk.ID.From)) == id {
						path, k = p, rk
						break found
					}
				}
			}
		}
		if _, ok := s.store[path]; !ok {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if o, ok := s.owner[path]; ok && o.kind != k && marksShared(k) {
			// What it marks is in another kind's object (Cloudflare's
			// email routing DNS, turned off in the zone's email routing
			// settings): that object stays, saying there is none.
			if !markAbsent(k, s.store[path]) {
				http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if !s.carries(w, rt.op, s.store[path], body) {
			return
		}
		if len(s.prepared[path]) < len(k.BeforeDelete) {
			// GitLab: an environment that is not stopped cannot be deleted.
			http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
			return
		}
		delete(s.prepared, path)
		delete(s.store, path)
		if o, ok := s.owner[path]; ok && o.kind.Shape == descriptor.CRUD {
			s.deleted[path] = o.kind
		}
		delete(s.owner, path)
		w.WriteHeader(http.StatusNoContent)
	default: // POST (upsert create), PUT, or PATCH on an item
		obj, ok := s.store[path]
		if req.Method == http.MethodPost && k.Shape == descriptor.Upsert && k.Create != nil && rt.op == k.Create && marksShared(k) {
			if p, in := samePlaceholders(rt.re, k.Create.Path, path, k.Read.Path); in && s.owner[p].kind != nil && s.owner[p].kind != k {
				// Made in another kind's object (Cloudflare's email routing
				// DNS, turned on in the zone's email routing settings): that
				// object, with this kind's fields, says there is one.
				if !k.Read.IsAbsent(withMarks([]*descriptor.Kind{k}, s.store[p])) {
					http.Error(w, `{"message":"already exists"}`, http.StatusBadRequest)
					return
				}
				mergeInto(s.store[p], body)
				for f := range k.Read.Absent {
					if v, ok := k.ServerFields[f]; ok {
						setPath(s.store[p], strings.ReplaceAll(f, ".", "/"), v)
					}
				}
				s.reply(w, http.StatusCreated, k, s.store[p])
				return
			}
		}
		if c, u := k.Create, k.Update; k.Shape == descriptor.Upsert && c != nil && rt.op == c && u != nil && u.Method == c.Method && u.Path == c.Path {
			// One call makes and replaces it (Atlas's
			// :grantMongoDBEmployeeAccess): where it is there, an update.
			at := path
			if k.Read.Find == nil && c.Path != k.ItemPath() {
				if p, moved := samePlaceholders(rt.re, c.Path, path, k.ItemPath()); moved {
					at = p
				}
			}
			if stored, has := s.store[at]; has {
				upd := *rt
				upd.op = u
				rt, path, obj, ok = &upd, at, stored, true
			}
		}
		if req.Method == http.MethodPost && k.Shape == descriptor.Upsert && k.Create != nil && rt.op == k.Create {
			if ok {
				http.Error(w, `{"message":"already exists"}`, http.StatusBadRequest)
				return
			}
			obj = map[string]any{}
			for f, v := range body {
				obj[f] = v
			}
			vals := routeParams(rt.re, rt.path, path)
			if p, moved := createdAt(k, rt, path, body); moved {
				// Made on a collection, named in its body (GitHub's
				// POST .../git/refs of ref "refs/heads/<branch>"): kept
				// at its item path.
				if _, ok := s.store[p]; ok {
					http.Error(w, `{"message":"already exists"}`, http.StatusUnprocessableEntity)
					return
				}
				path = p
				vals = routeParams(compile(k.ItemPath()), k.ItemPath(), p)
			}
			s.fillFindField(k, rt, path, obj)
			s.fillCarried(k, obj)
			if fd := k.Read.Find; fd != nil && fd.Key != "" && k.Delete != nil {
				// Found by its key in an object of them (Vault's secrets sync
				// association): kept where it is deleted.
				if p, ok := samePlaceholders(rt.re, k.Create.Path, path, k.Delete.Path); ok {
					path = p
				}
			} else if k.Read.Find == nil && k.Create.Path != k.ItemPath() {
				// Made on another path than it is kept at (Vault's KMIP CA,
				// made at .../ca/{name}/generate and read at .../ca/{name}).
				if p, ok := samePlaceholders(rt.re, k.Create.Path, path, k.ItemPath()); ok {
					path = p
				}
			} else if len(k.Create.Body) > 0 {
				// Named in the create's body, made on its collection (Okta's
				// trusted server: {"trusted": ["{associatedServerId}"]}): kept
				// at its item path, with the field its list is read by.
				templateValues(k.Create.Body, body, vals)
				at := k.ItemPath()
				if fd := k.Read.Find; fd != nil && k.Delete != nil && strings.Contains(k.Delete.Path, "{"+fd.Param+"}") {
					at = k.Delete.Path // read from a list: kept where it is deleted
				}
				if p, err := rest.Expand(at, vals); err == nil {
					path = p
				}
				if fd := k.Read.Find; fd != nil && fd.Field != "" && vals[fd.Param] != "" {
					setPath(obj, strings.ReplaceAll(fd.Field, ".", "/"), vals[fd.Param])
				}
			}
			s.store[path] = obj
			s.owner[path] = owned{k, vals}
			s.reply(w, http.StatusCreated, k, obj)
			return
		}
		if !ok && (k.Shape == descriptor.CRUD || (k.Shape == descriptor.Upsert && k.Create != nil)) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
			return
		}
		if ok && !s.carries(w, rt.op, obj, body) {
			return
		}
		for _, f := range k.CreateOnly {
			if f == k.UpdateID {
				continue // the update names the item by it (a rules file by its name)
			}
			if _, has := descriptor.PickField(body, f); has && ok && (rt.op == nil || !containsString(rt.op.Carry, f)) {
				// As PagerDuty does: an update that names a create-only field is refused.
				http.Error(w, `{"message":"`+f+`: doesn't match any valid key"}`, http.StatusBadRequest)
				return
			}
		}
		if !ok || req.Method == http.MethodPut && k.Shape == descriptor.Upsert {
			old := obj
			obj = map[string]any{}
			for _, f := range k.CreateOnly {
				// A replace keeps what only the create sets (an upsert's PUT
				// leaves out Vault's ACME account directory_url).
				if sub, has := descriptor.PickField(old, f); has {
					mergeInto(obj, sub)
				}
			}
		}
		mergeInto(obj, body)
		if !ok {
			s.fillCarried(k, obj)
		} else if rt.op == k.Update {
			s.bumpCarried(k, obj)
		}
		s.fillFindField(k, rt, path, obj)
		s.store[path] = obj
		if _, has := s.owner[path]; !has {
			s.owner[path] = owned{k, routeParams(rt.re, rt.path, path)}
		}
		s.replyOp(w, http.StatusOK, k, rt.op, obj)
	}
}

func cands(rt *route, also []*route) []*route { return append([]*route{rt}, also...) }

// listsOn is the answer to a list read several kinds find their objects in,
// each at its own list (Find.List), where they differ: every listing
// kind's objects stored with the path's parameters. Nil where one list
// answers for all, or the path is an item's.
func (s *Server) listsOn(rs []*route, listed map[*descriptor.Kind]bool, path string) map[string]any {
	if len(listed) < 2 {
		return nil
	}
	if _, ok := s.owner[path]; ok {
		return nil
	}
	at := map[string]bool{}
	for k := range listed {
		at[k.ListAt()] = true
	}
	if len(at) < 2 || at[""] {
		return nil
	}
	out := map[string]any{}
	for _, r := range rs {
		if r.role != list {
			continue
		}
		want := routeParams(r.re, r.path, path)
		items := []any{}
		for key, obj := range s.store {
			o, ok := s.owner[key]
			if !ok || o.kind != r.kind {
				continue
			}
			match := true
			for n, v := range want {
				if got, has := o.params[n]; has && got != v {
					match = false
				}
			}
			if match {
				items = append(items, obj)
			}
		}
		sortByKey(items)
		setPath(out, r.kind.ListAt(), items)
	}
	return out
}

// writtenWhereListed: a find kind with no create, updated on the very path
// it is read from (a team's one EMU group mapping; a GitLab user's
// identity, PUT /users/{id}): the item there is the list's element. Not a
// kind named by a body field on its collection (keyedBy), which a create
// makes there and the fake keeps by that name.
func writtenWhereListed(k *descriptor.Kind) bool {
	return k.Read.Find != nil && k.Create == nil && k.Update != nil && k.Update.Path == k.Read.Path
}

// holds reports whether the object stored at p is the one a find kind's
// parameters name: its find field is the find parameter's value.
func (s *Server) holds(k *descriptor.Kind, p string, params map[string]string) bool {
	obj, ok := s.store[p]
	fd := k.Read.Find
	if !ok || fd == nil {
		return false
	}
	v := descriptor.Dig(obj, strings.ReplaceAll(fd.Field, ".", "/"))
	return fd.Field == "" || v != nil && strings.EqualFold(descriptor.Text(v), params[fd.Param])
}

// pick chooses among routes of several kinds on one URL: where a CRUD item
// was deleted and nothing is stored since, its kind (it answers 404); else
// a find read's list (it answers for the item it is in too); else the kind that made what is
// stored there; with nothing stored, a settings kind (which answers before
// anything is written), else the first.
func (s *Server) pick(rs []*route, path string) *route {
	for _, r := range rs {
		if r.role == setList {
			return r // every element under the path, whichever kind made it
		}
	}
	if _, stored := s.store[path]; !stored {
		if k := s.deleted[path]; k != nil {
			// A CRUD item deleted here is gone, though a settings kind
			// shares the path (Datadog's custom logs pipelines and its
			// integration pipelines, taken by their IDs), and so is any
			// list another kind finds in it (a GitLab user deleted at
			// /users/{id}, where its identities are listed and its avatar
			// set).
			for _, r := range rs {
				if r.kind == k {
					return r
				}
			}
		}
	}
	for _, r := range rs {
		if r.role == list {
			return r // a list in another kind's item: the list's route answers both (see list)
		}
	}
	if o, ok := s.owner[path]; ok {
		for _, r := range rs {
			if r.kind == o.kind {
				return r
			}
		}
	}
	for _, r := range rs {
		// A mixed kind's object is kept under its GraphQL key, not its
		// REST path: the path is still its own (GitHub's
		// EnterpriseOrganization, made by GraphQL and changed by PATCH
		// /orgs/{id}, where OrganizationSettings reads and writes too).
		if r.kind.IsMixed() {
			if _, ok := s.store[GraphQLKey(r.kind, routeParams(r.re, r.path, path))]; ok {
				return r
			}
		}
	}
	if _, stored := s.store[path]; !stored {
		for _, r := range rs {
			// A kind read on another kind's path and kept where it is
			// written (Cloudflare's email routing DNS, read in the zone's
			// email routing settings): its object, where it has made one,
			// else what the path answers when there is none (Read.Absent:
			// enabled false).
			if r.method == http.MethodGet && r.op == &r.kind.Read && r.kind.ReadsElsewhere() {
				if _, ok := s.ownedBy(r.kind, routeParams(r.re, r.path, path)); ok || len(r.kind.Read.Absent) > 0 {
					return r
				}
			}
		}
		for _, r := range rs {
			if r.kind.Shape == descriptor.Settings && !s.riders[r.kind] {
				return r // a rider's resource is another kind's: not there
			}
		}
		for _, r := range rs {
			// A field of another kind's item that its write sets (a
			// GitLab user's identity, by PUT /users/{id}): a CRUD item is
			// not made by a write to its own path.
			if r.kind.Shape == descriptor.Upsert {
				return r
			}
		}
	}
	for _, r := range rs {
		if r.kind.Element == nil {
			return r // an Element kind answers only for what holds its sample (seed)
		}
	}
	return rs[0]
}

// marksShared reports whether k is a mark in what its read answers: it is
// made and deleted on other paths than it is read at, and the read says
// whether it is there (Read.Absent). Cloudflare's email routing DNS, turned
// on by POST and off by DELETE .../email/routing/dns, is enabled or not in
// the zone's email routing settings, GET .../email/routing, another kind's.
// The fake keeps it in that kind's object where there is one.
func marksShared(k *descriptor.Kind) bool {
	return len(k.Read.Absent) > 0 && k.Read.Find == nil && k.Shape == descriptor.Upsert &&
		k.Create != nil && k.Create.Path != k.Read.Path && k.Delete != nil && k.Delete.Path != k.Read.Path
}

// withMarks is obj with each marking kind's absent values where obj has
// none of its own: another kind's object, in which none of them was made.
func withMarks(marks []*descriptor.Kind, obj map[string]any) map[string]any {
	out, cloned := obj, false
	for _, k := range marks {
		for f, v := range k.Read.Absent {
			at := strings.ReplaceAll(f, ".", "/")
			if v != nil && descriptor.Dig(out, at) == nil {
				if !cloned {
					out, cloned = deepClone(obj), true
				}
				setPath(out, at, v)
			}
		}
	}
	return out
}

// markAbsent sets k's absent values in obj (a null one: the field goes),
// reporting whether k was there to remove.
func markAbsent(k *descriptor.Kind, obj map[string]any) bool {
	if k.Read.IsAbsent(withMarks([]*descriptor.Kind{k}, obj)) {
		return false
	}
	for f, v := range k.Read.Absent {
		at := strings.ReplaceAll(f, ".", "/")
		if v != nil {
			setPath(obj, at, v)
			continue
		}
		parent, name := obj, at
		if i := strings.LastIndex(at, "/"); i >= 0 {
			parent, _ = descriptor.Dig(obj, at[:i]).(map[string]any)
			name = at[i+1:]
		}
		if parent != nil {
			delete(parent, name)
		}
	}
	return true
}

// withServerFields is obj with the fields the API sets itself (the kind's
// ServerFields) where obj has none of its own.
func withServerFields(k *descriptor.Kind, obj map[string]any) map[string]any {
	if len(k.ServerFields) == 0 || k.BodyField != "" {
		return obj
	}
	out := make(map[string]any, len(obj)+len(k.ServerFields))
	for f, v := range k.ServerFields {
		out[f] = v
	}
	for f, v := range obj {
		out[f] = v
	}
	return out
}

// idText is an ID as a path holds it: a JSON number with no fraction as an
// integer (270000022, not 2.70000022e+08).
func idText(id any) string { return descriptor.Text(id) }

func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				mergeInto(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}

// withoutWriteOnly copies obj less its write-only fields, at any depth
// (settings.clientSecret).
func withoutWriteOnly(k *descriptor.Kind, at string, obj map[string]any) map[string]any {
	out := map[string]any{}
	for f, v := range obj {
		p := f
		if at != "" {
			p = at + "." + f
		}
		if k.IsWriteOnly(p) {
			continue
		}
		if m, ok := v.(map[string]any); ok {
			v = withoutWriteOnly(k, p, m)
		}
		out[f] = v
	}
	return out
}

// samePlaceholders expands to the placeholders path matched in from.
func samePlaceholders(re *regexp.Regexp, from, path, to string) (string, bool) {
	m := re.FindStringSubmatch(path)
	names := placeholderRE.FindAllStringSubmatch(from, -1)
	if m == nil || len(m) != len(names)+1 {
		return "", false
	}
	vals := map[string]string{}
	for i, n := range names {
		v, err := url.PathUnescape(m[i+1])
		if err != nil {
			return "", false
		}
		vals[n[1]] = v
	}
	p, err := rest.Expand(to, vals)
	return p, err == nil
}

// prepare records a BeforeDelete call on the item it names (the call's
// placeholders are the read path's).
func (s *Server) prepare(w http.ResponseWriter, rt *route, path string) {
	k := rt.kind
	item, ok := samePlaceholders(rt.re, k.BeforeDelete[rt.prep].Path, path, k.Read.Path)
	if _, found := s.store[item]; !ok || !found {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		return
	}
	if s.prepared[item] == nil {
		s.prepared[item] = map[int]bool{}
	}
	s.prepared[item][rt.prep] = true
	s.reply(w, http.StatusOK, k, s.store[item])
}

// readBack is obj as the API reads it back: fields with a Compare rule in
// the API's own form (splitSpace: the words of a string, as an array;
// enabledObject: the value as {"enabled": value}; slugObject: the value as
// {"slug": value, "id": ...}; nodeIDs: the IDs as {"nodes": [{"id": ...}]},
// in another order).
// readBackAt is v with the value at path (field names, "[]" for each
// element of a list) in the form rule says the API reads it back in.
func readBackAt(path []string, rule string, v any) any {
	if len(path) == 0 || path[0] == "" {
		tmp := readBack(&descriptor.Kind{Compare: map[string]string{"v": rule}}, map[string]any{"v": v})
		return tmp["v"]
	}
	if path[0] == "[]" {
		l, ok := v.([]any)
		if !ok {
			return v
		}
		out := make([]any, len(l))
		for i, e := range l {
			out[i] = readBackAt(path[1:], rule, e)
		}
		return out
	}
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	if _, has := m[path[0]]; !has {
		return v
	}
	out := clone(m)
	out[path[0]] = readBackAt(path[1:], rule, m[path[0]])
	return out
}

func readBack(k *descriptor.Kind, obj map[string]any) map[string]any {
	if len(k.Compare) == 0 {
		return obj
	}
	out := clone(obj)
	for f, rule := range k.Compare {
		if strings.ContainsAny(f, ".[") {
			// Inside objects and lists' elements (GitLab's value stream
			// stages[].startEventLabelId): each value there in its read form.
			top, rest, _ := strings.Cut(strings.ReplaceAll(f, "[]", ".[]"), ".")
			if v, ok := out[top]; ok {
				out[top] = readBackAt(strings.Split(rest, "."), rule, v)
			}
			continue
		}
		if v, ok := out[f]; ok && v != nil && rule == descriptor.CompareEnabledObject {
			out[f] = map[string]any{"enabled": v}
		}
		if l, ok := out[f].([]any); ok && rule == descriptor.CompareUnordered {
			rev := make([]any, len(l))
			for i, e := range l {
				rev[len(l)-1-i] = e
			}
			out[f] = rev
		}
		if l, ok := out[f].([]any); ok && rule == descriptor.CompareNodeIDs {
			nodes := make([]any, len(l))
			for i, e := range l {
				nodes[len(l)-1-i] = map[string]any{"id": e}
			}
			out[f] = map[string]any{"nodes": nodes}
		}
		if l, ok := out[f].([]any); ok && rule == descriptor.CompareSubsetByID {
			// Every element the API has, the written ones among them, last first.
			all := []any{map[string]any{"id": "fake-other", "enabled": false}}
			for i := len(l) - 1; i >= 0; i-- {
				all = append(all, l[i])
			}
			out[f] = all
		}
		if str, ok := out[f].(string); ok && rule == descriptor.CompareSlugObject {
			out[f] = map[string]any{"slug": str, "id": "1"}
		}
		if str, ok := out[f].(string); ok && rule == descriptor.CompareSplitSpace {
			words := []any{}
			for _, w := range strings.Fields(str) {
				words = append(words, w)
			}
			out[f] = words
		}
	}
	return out
}

// reply answers as the real API would: wrapped in the envelope, without
// write-only fields, and with Compare fields in the form the API reads back.
func (s *Server) reply(w http.ResponseWriter, status int, k *descriptor.Kind, obj map[string]any) {
	s.replyOp(w, status, k, nil, obj)
}

// replyOp is reply for one call, which may answer in an envelope of its own
// (Op.Envelope: Cloudflare's bulk create, {"result": [{...}]}).
func (s *Server) replyOp(w http.ResponseWriter, status int, k *descriptor.Kind, op *descriptor.Op, obj map[string]any) {
	var resp any = withoutWriteOnly(k, "", readBack(k, withServerFields(k, obj)))
	if k.Order != nil {
		// The order as the list it is read from (last first where the API
		// orders by a field: the reader sorts).
		resp = k.Order.Answer(resp.(map[string]any), k.Order.SortBy != "")
	}
	if op != nil && op.Envelope != "" {
		resp = descriptor.Wrap(op.Envelope, resp)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(resp)
		return
	}
	if k.BodyField != "" {
		arr, _ := resp.(map[string]any)[k.BodyField].([]any)
		if arr == nil {
			arr = []any{}
		}
		resp = arr
	}
	if k.Envelope != "" {
		parts := strings.Split(k.Envelope, "/")
		for i := len(parts) - 1; i >= 0; i-- {
			resp = map[string]any{parts[i]: resp}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

func decodeMultipart(req *http.Request, ct string, k *descriptor.Kind) (map[string]any, error) {
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		return nil, err
	}
	mr := multipart.NewReader(req.Body, params["boundary"])
	body := map[string]any{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			if k.Multipart != nil {
				for _, f := range k.Multipart.Files {
					if _, ok := body[f]; !ok {
						if _, isSlice := k.Sample[f].([]any); isSlice {
							body[f] = []any{}
						}
					}
				}
			}
			return body, nil
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(part)
		if err != nil {
			return nil, err
		}
		name := part.FormName()
		filename := part.FileName()
		partCT := part.Header.Get("Content-Type")
		if filename != "" || k.Multipart.IsFile(name) {
			f := map[string]any{"name": filename, "contentType": partCT}
			if utf8.Valid(data) {
				f["content"] = string(data)
			} else {
				f["contentBase64"] = base64.StdEncoding.EncodeToString(data)
			}
			if _, isSingle := k.Sample[name].(map[string]any); isSingle {
				body[name] = f
			} else {
				arr, _ := body[name].([]any)
				body[name] = append(arr, f)
			}
			continue
		}
		if strings.Contains(partCT, "json") {
			var v any
			if err := json.Unmarshal(data, &v); err != nil {
				return nil, err
			}
			body[name] = v
			continue
		}
		body[name] = string(data)
	}
}

func clone(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

// deepClone copies a decoded JSON object, its objects and lists too.
func deepClone(m map[string]any) map[string]any {
	var cp func(v any) any
	cp = func(v any) any {
		switch t := v.(type) {
		case map[string]any:
			out := make(map[string]any, len(t))
			for k, e := range t {
				out[k] = cp(e)
			}
			return out
		case []any:
			out := make([]any, len(t))
			for i, e := range t {
				out[i] = cp(e)
			}
			return out
		}
		return v
	}
	out, _ := cp(m).(map[string]any)
	return out
}

func setPath(m map[string]any, path string, v any) {
	parts := strings.Split(path, "/")
	for i, p := range parts[:len(parts)-1] {
		if _, err := strconv.Atoi(parts[i+1]); err == nil {
			if _, has := m[p]; !has {
				// A list, as the API answers one (imported_issuers/0: the
				// first issuer a PKI import made).
				m[p] = descriptor.Wrap(strings.Join(parts[i+1:], "/"), v)
				return
			}
		}
		next, ok := m[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[p] = next
		}
		m = next
	}
	m[parts[len(parts)-1]] = v
}

// AssocKey is where the fake keeps one assoc link: its kind and body.
func AssocKey(k *descriptor.Kind, body map[string]any) string {
	b, _ := json.Marshal(body)
	return "assoc:" + k.Kind + ":" + string(b)
}

// keyedList answers a keyed find's read (descriptor.Find.Key): every object
// of its kind whose parameters agree with the path's, in an object keyed by
// the key's template filled from the parameters it was made with.
func (s *Server) keyedList(w http.ResponseWriter, rt *route, path string) {
	k := rt.kind
	want := routeParams(rt.re, rt.path, path)
	out := map[string]any{}
	for key, obj := range s.store {
		o, ok := s.owner[key]
		if !ok || o.kind != k || !agree(o.params, want) {
			continue
		}
		if id, ok := k.Read.Find.KeyIn(o.params); ok {
			out[id] = obj
		}
	}
	var resp any = out
	if at := k.ListAt(); at != "" {
		resp = descriptor.Wrap(at, out)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) replyList(w http.ResponseWriter, at string, items []any) {
	var resp any = items
	if items == nil {
		resp = []any{}
	}
	if at != "" {
		parts := strings.Split(at, "/")
		for i := len(parts) - 1; i >= 0; i-- {
			if parts[i] == "*" {
				resp = []any{resp} // each element's list (descriptor.DigList): one element holds them all
				continue
			}
			resp = map[string]any{parts[i]: resp}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func sortByKey(items []any) {
	sort.Slice(items, func(i, j int) bool {
		a, _ := json.Marshal(items[i])
		b, _ := json.Marshal(items[j])
		return string(a) < string(b)
	})
}

var placeholderRE = regexp.MustCompile(`\{\+?([^}]+)\}`)

// fillFindField gives an item a find read looks for by a path parameter
// that field, as the API's list answers it (a member's user.id).
func (s *Server) fillFindField(k *descriptor.Kind, rt *route, path string, obj map[string]any) {
	fd := k.Read.Find
	if fd == nil || fd.Field == "" {
		return
	}
	at := strings.ReplaceAll(fd.Field, ".", "/")
	if descriptor.Dig(obj, at) != nil {
		return
	}
	if v, found := pathParam(rt.re, rt.path, path, fd.Param); found {
		setPath(obj, at, v)
	}
}

// findItemPath is where a find kind's new item lives: its update or delete
// path, filled from the create's path, the new ID, and body fields named
// as the remaining placeholders.
func findItemPath(k *descriptor.Kind, rt *route, path, id string, body map[string]any) (string, bool) {
	var op *descriptor.Op
	for _, o := range []*descriptor.Op{k.Delete, k.Update} {
		if o != nil && strings.Contains(o.Path, "{"+k.Read.Find.Param+"}") {
			op = o
			break
		}
	}
	if op == nil {
		return "", false
	}
	vals := routeParams(rt.re, rt.path, path)
	vals[k.Read.Find.Param] = id
	if k.ID != nil {
		vals[k.ID.Param] = id
	}
	for _, n := range placeholderRE.FindAllStringSubmatch(op.Path, -1) {
		if _, ok := vals[n[1]]; !ok {
			// A body field, perhaps under another name (Confluent's entityType fills {typeName}).
			v := bodyParam(k, n[1], body)
			if v == nil {
				return "", false
			}
			vals[n[1]] = idText(v)
		}
	}
	p, err := rest.Expand(op.Path, vals)
	return p, err == nil
}

// routeParams is every placeholder a path matched in a route's template.
func routeParams(re *regexp.Regexp, tmpl, path string) map[string]string {
	out := map[string]string{}
	m := re.FindStringSubmatch(path)
	names := placeholderRE.FindAllStringSubmatch(tmpl, -1)
	if m == nil || len(m) != len(names)+1 {
		return out
	}
	for i, n := range names {
		if v, err := url.PathUnescape(m[i+1]); err == nil {
			out[n[1]] = v
		}
	}
	return out
}

// pathParam extracts the value of a named placeholder from path.
func pathParam(re *regexp.Regexp, tmpl, path, name string) (string, bool) {
	m := re.FindStringSubmatch(path)
	names := placeholderRE.FindAllStringSubmatch(tmpl, -1)
	if m == nil || len(m) != len(names)+1 {
		return "", false
	}
	for i, n := range names {
		if n[1] == name {
			v, err := url.PathUnescape(m[i+1])
			return v, err == nil
		}
	}
	return "", false
}

// itemPath expands the kind's read path from the placeholders the create request
// matched plus the new item's id, and any other from the body field that
// fills it (Datadog's custom framework is made at .../custom_frameworks and
// read at .../custom_frameworks/{handle}/{version}, its body's
// attributes.handle).
func itemPath(k *descriptor.Kind, re *regexp.Regexp, path, id string, body map[string]any) (string, bool) {
	m := re.FindStringSubmatch(path)
	names := placeholderRE.FindAllStringSubmatch(k.Create.Path, -1)
	if m == nil || len(m) != len(names)+1 {
		return "", false
	}
	vals := map[string]string{k.ID.Param: id}
	for i, n := range names {
		v, err := url.PathUnescape(m[i+1])
		if err != nil {
			return "", false
		}
		vals[n[1]] = v
	}
	for _, n := range placeholderRE.FindAllStringSubmatch(k.ItemPath(), -1) {
		if _, ok := vals[n[1]]; !ok && namedInBody(k, k.Create) {
			if v := bodyParam(k, n[1], body); v != nil {
				vals[n[1]] = idText(v)
			}
		}
	}
	p, err := rest.Expand(k.ItemPath(), vals)
	return p, err == nil
}

// fillCarried gives a new object the fields an update or a delete carries
// back (Op.Carry) that the API sets itself (a resourceVersion, a blob sha),
// where the body did not: the fake's own values. The ID field is set by
// the create.
func (s *Server) fillCarried(k *descriptor.Kind, obj map[string]any) {
	if k.Delete != nil {
		// What a delete path names it by, as the API answers it (a k6
		// schedule's own ID: /schedules/{current.id}).
		for _, n := range descriptor.Placeholders(k.Delete.Path) {
			at := strings.ReplaceAll(strings.TrimPrefix(n, "current."), ".", "/")
			if strings.HasPrefix(n, "current.") && descriptor.Dig(obj, at) == nil && descriptor.Dig(k.ServerFields, at) == nil {
				s.nextID++
				setPath(obj, at, s.nextID)
			}
		}
	}
	for _, op := range []*descriptor.Op{k.Update, k.Delete} {
		if op == nil {
			continue
		}
		for _, f := range op.Carry {
			at := strings.ReplaceAll(f, ".", "/")
			if descriptor.Dig(obj, at) != nil || (k.ID != nil && at == k.ID.From) {
				continue
			}
			s.nextID++
			setPath(obj, at, idText(s.nextID))
		}
	}
}

// bumpCarried gives an updated object new values of the fields its update
// carries, but its ID and create-only ones, as an API moves a version on (a resourceVersion, a
// blob sha): a write must send what a read answered just before.
func (s *Server) bumpCarried(k *descriptor.Kind, obj map[string]any) {
	for _, f := range k.Update.Carry {
		at := strings.ReplaceAll(f, ".", "/")
		if k.ID != nil && at == k.ID.From || k.IsCreateOnly(f) {
			continue // what names it, not a version (an authenticator's key)
		}
		s.nextID++
		setPath(obj, at, idText(s.nextID))
	}
}

// carries checks that a write to stored sends each field op carries as the
// API has it, as an API with optimistic concurrency does (Grafana's 409:
// "Provided version ” of route ... does not match current version"); it
// answers 409 and reports false when one is missing or differs.
func (s *Server) carries(w http.ResponseWriter, op *descriptor.Op, stored, body map[string]any) bool {
	if op == nil {
		return true
	}
	for _, f := range op.Carry {
		at := strings.ReplaceAll(f, ".", "/")
		want := descriptor.Dig(stored, at)
		if want == nil {
			continue
		}
		if got := descriptor.Dig(body, at); got == nil || idText(got) != idText(want) {
			b, _ := json.Marshal(map[string]any{"message": "Provided " + f + " " + idText(got) + " does not match current " + idText(want)})
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write(b)
			return false
		}
	}
	return true
}

// ownedBy is the object kind k keeps whose path parameters agree with want.
func (s *Server) ownedBy(k *descriptor.Kind, want map[string]string) (map[string]any, bool) {
	keys := make([]string, 0, len(s.store))
	for key := range s.store {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		o, ok := s.owner[key]
		if !ok || o.kind != k {
			continue
		}
		match := true
		for n, v := range want {
			if got, has := o.params[n]; has && got != v {
				match = false
			}
		}
		if match {
			return s.store[key], true
		}
	}
	return nil, false
}

// adoptList answers an adopting kind's list (Kind.Adopt): the objects of
// every kind that adopts from this list whose parameters agree; a kind with
// none has one there already, made from its sample with the fields
// Adopt.Match names set, as the vendor makes its defaults (Okta's default
// authorization server).
func (s *Server) adoptList(w http.ResponseWriter, rt *route, path string) {
	want := routeParams(rt.re, rt.path, path)
	var kinds []*descriptor.Kind // every kind that adopts from this list
	for i := range s.routes {
		r := &s.routes[i]
		if r.role == adoptList && r.path == rt.path && sameQuery(r.query, rt.query) {
			kinds = append(kinds, r.kind)
		}
	}
	var out []any
	seen := map[string]bool{}
	for _, k := range kinds {
		s.adoptable(k, kinds, want)
		for key, obj := range s.store {
			if o, ok := s.owner[key]; ok && o.kind == k && agree(o.params, want) && !seen[key] {
				seen[key] = true
				out = append(out, obj)
			}
		}
	}
	sortByKey(out)
	at := ""
	if rt.kind.Adopt.Find != nil {
		at = rt.kind.Adopt.Find.List
	}
	s.replyList(w, at, out)
}

// adoptable makes sure kind k has an object to adopt among those of kinds
// whose parameters agree with want: one matching its sample, else one made
// from its sample with the fields Adopt.Match names set.
func (s *Server) adoptable(k *descriptor.Kind, kinds []*descriptor.Kind, want map[string]string) {
	isParam := map[string]bool{}
	for _, p := range k.Params {
		isParam[p.Field] = true
	}
	obj := map[string]any{}
	for f, v := range k.Sample {
		if !isParam[f] {
			obj[f] = v
		}
	}
	for key, have := range s.store {
		o, ok := s.owner[key]
		if !ok || !agree(o.params, want) {
			continue
		}
		for _, other := range kinds {
			if o.kind == other && k.Adopt.Matches(have, obj) {
				return
			}
		}
	}
	for f, v := range k.Adopt.Match {
		if !descriptor.IsTemplate(v) {
			setPath(obj, strings.ReplaceAll(f, ".", "/"), v)
		}
	}
	s.nextID++
	id := idText(s.nextID)
	setPath(obj, k.ID.From, id)
	s.fillCarried(k, obj)
	vals := map[string]string{k.ID.Param: id}
	for n, v := range want {
		vals[n] = v
	}
	for _, p := range k.Params {
		if _, ok := vals[p.Name]; !ok {
			vals[p.Name] = idText(k.Sample[p.Field])
		}
	}
	if key, err := rest.Expand(k.ItemPath(), vals); err == nil {
		s.store[key] = obj
		s.owner[key] = owned{k, vals}
	}
}

// agree reports whether the parameters an object was made with agree with
// want wherever both name one.
func agree(have, want map[string]string) bool {
	for n, v := range want {
		if got, ok := have[n]; ok && got != v {
			return false
		}
	}
	return true
}

func sameQuery(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func containsString(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// namedByCurrent is where the kind's object is kept whose fields (as the API
// answers them, server fields too) are what a delete path's
// {current.field} placeholders matched (a k6 schedule, deleted at
// /schedules/{current.id}).
func (s *Server) namedByCurrent(k *descriptor.Kind, vals map[string]string) (string, bool) {
	keys := make([]string, 0, len(s.store))
	for key := range s.store {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if o, ok := s.owner[key]; !ok || o.kind != k {
			continue
		}
		obj := withServerFields(k, s.store[key])
		match := false
		for n, v := range vals {
			if !strings.HasPrefix(n, "current.") {
				continue
			}
			got := descriptor.Dig(obj, strings.ReplaceAll(strings.TrimPrefix(n, "current."), ".", "/"))
			match = got != nil && idText(got) == v
			if !match {
				break
			}
		}
		if match {
			return key, true
		}
	}
	return "", false
}
