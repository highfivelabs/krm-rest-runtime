// SPDX-License-Identifier: Apache-2.0

// Package descriptor describes how a Kubernetes kind maps onto a vendor's REST
// API: which calls create, read, update and delete it, which spec fields fill
// the URL's path parameters, and which go in the request body. A vendor
// repository generates one descriptor per kind into types.yaml; the generic
// reconciler and the fake API server both read it.
package descriptor

import (
	"fmt"
	"math"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// Shape is how a resource's lifecycle looks in the API.
type Shape string

const (
	// CRUD: POST to a collection creates it and the server assigns an ID;
	// GET, PATCH or PUT, and DELETE act on the item path.
	CRUD Shape = "crud"
	// Upsert: the caller names the item; PUT on the item path creates or
	// replaces it, GET reads it, DELETE removes it.
	Upsert Shape = "upsert"
	// Settings: a singleton that always exists; GET reads it, PATCH or PUT
	// changes it, and deleting the object leaves the settings as they are.
	Settings Shape = "settings"
	// Assoc: a link with no identity of its own (a grant, a membership).
	// Create sends the spec; Read lists the links and finds one holding every
	// spec field; Delete sends the same body to undo it (Snowflake's
	// grants:revoke).
	Assoc Shape = "assoc"
	// SetShape: the whole set of a parent's elements, each its own API
	// object (a repository's labels, an app installation's repositories):
	// the spec lists every element (Kind.Set), and the reconciler adds the
	// ones missing (Create, one call each), changes the ones that differ
	// (Update), and removes the ones the spec leaves out (Delete), until
	// the API's list (Read) holds exactly those. Deleting the object removes
	// its elements. Terraform's github_issue_labels beside the one-label
	// github_issue_label.
	SetShape Shape = "set"
)

// Set says what a kind of shape set manages: the spec field that lists the
// elements, and how one is named.
type Set struct {
	// Field is the spec field holding every element: objects, or bare
	// values (an installation's repository IDs) that are each the Key.
	Field string `json:"field"`
	// Key is the element field that names one, as the API's list answers
	// it (a label's name, a repository's id).
	Key string `json:"key"`
	// Param is the path parameter of the element calls (Create, Update,
	// Delete) the key fills (a label's {name}; {repository_id} for a
	// repository's id). Other element placeholders are filled from the
	// element's field of that name.
	Param string `json:"param"`
	// List is where in the read's answer the list is, as a slash path; ""
	// means the answer is the list.
	List string `json:"list,omitempty"`
	// KeyURL: the list names an element only by a URL, at this slash path
	// in it, whose last segment is the key (Okta's linked objects:
	// {"_links": {"self": {"href": ".../api/v1/users/<id>"}}}). Key is then
	// the name the key has in the spec's elements.
	KeyURL string `json:"keyURL,omitempty"`
	// ReadEach, where the list holds only the elements' keys (Vault's LIST:
	// {"data": {"keys": ["a", "b"]}}): the call that reads one element, on
	// a path naming it by {Param}; its Envelope is where the answer holds it.
	ReadEach *Op `json:"readEach,omitempty"`
	// Holder: the resource the elements are in, which the kind makes before
	// adding them and deletes, with them, when the object is deleted (a
	// Vault database secrets mount and its connections, Terraform's
	// vault_database_secrets_mount).
	Holder *Holder `json:"holder,omitempty"`
	// Keys, in place of Key and Param: the element fields that name one
	// element together (Atlas's managed namespace: its db and collection).
	// Each fills the {field} placeholders of the element calls' paths and
	// query values (DELETE .../managedNamespaces?db={db}&collection={collection}).
	Keys []string `json:"keys,omitempty"`
	// Batch: a second list of the same resource, read in the same answer
	// and written another way (Batch).
	Batch *Batch `json:"batch,omitempty"`
}

// Batch is a second list a set kind's resource holds (Set.Batch), whose
// elements are added by one call that sends only the new ones and are
// removed only all together: Atlas's custom zone mappings of a global
// cluster, POST .../customZoneMapping {"customZoneMappings": [...]} and
// DELETE .../customZoneMapping (Terraform's
// mongodbatlas_global_cluster_config custom_zone_mappings). A spec that
// leaves Field out leaves the list as it is; one that lists fewer
// elements than the API holds, or changes one, is refused unless it lists
// none (the clear), as Terraform refuses it.
type Batch struct {
	// Field is the spec field listing the elements, objects named by Key.
	Field string `json:"field"`
	Key   string `json:"key"`
	// At is where the read's answer holds the list, as a slash path: an
	// object whose names are the elements' Key and whose values are their
	// Value, in the form the API keeps it (Atlas answers a zone's ID where
	// the spec names the zone): a change of a Value is found against the
	// elements last applied, as Terraform finds it, and the API's form is
	// kept in status.atProvider at At.
	At    string `json:"at"`
	Value string `json:"value"`
	// Add sends {Field: [the elements the API lacks]}; Clear removes them
	// all (a 404 is done).
	Add   Op `json:"add"`
	Clear Op `json:"clear"`
}

// KeyFields are the element fields that name one element: Keys, else Key.
func (st *Set) KeyFields() []string {
	if len(st.Keys) > 0 {
		return st.Keys
	}
	return []string{st.Key}
}

// KeyNames names the key in messages ("name"; "db and collection").
func (st *Set) KeyNames() string {
	f := st.KeyFields()
	if len(f) == 1 {
		return f[0]
	}
	return strings.Join(f[:len(f)-1], ", ") + " and " + f[len(f)-1]
}

// KeyOf is the key of element m: its Key's value, or for Keys each
// field=value, URL query encoded in Keys' order (db=app&collection=users);
// false where one is missing or empty.
func (st *Set) KeyOf(m map[string]any) (string, bool) {
	if len(st.Keys) == 0 {
		v := m[st.Key]
		if v == nil || Text(v) == "" {
			return "", false
		}
		return Text(v), true
	}
	parts := make([]string, 0, len(st.Keys))
	for _, f := range st.Keys {
		v := m[f]
		if v == nil || Text(v) == "" {
			return "", false
		}
		parts = append(parts, neturl.QueryEscape(f)+"="+neturl.QueryEscape(Text(v)))
	}
	return strings.Join(parts, "&"), true
}

// namesElement reports whether every placeholder of template v is one of
// the set's key fields: an element call's value, filled per element.
func (st *Set) namesElement(v string) bool {
	if st == nil || len(st.Keys) == 0 {
		return false
	}
	names := Placeholders(v)
	for _, n := range names {
		if !containsField(st.Keys, n) {
			return false
		}
	}
	return len(names) > 0
}

// Holder is the resource a set kind's elements are in (Set.Holder).
type Holder struct {
	// Create makes it, sending Fields of the spec and its own Body (a
	// mount's type: database); Read says whether it is there (a 404 is
	// not); Delete removes it and the elements in it.
	Create Op `json:"create"`
	Read   Op `json:"read"`
	Delete Op `json:"delete"`
	// Fields are the spec fields the create sends. They are set when the
	// holder is made: a change to one is not sent.
	Fields []string `json:"fields,omitempty"`
}

// Op is one HTTP call. Path is relative to the connection's base URL and
// holds {param} placeholders.
type Op struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Headers are sent with this operation only, after the vendor's own
	// (MongoDB Atlas names each endpoint's API version in Accept and
	// Content-Type). A value may hold templates, as a query value may
	// (Kind.FillQuery, Op.HeadersOf); one left empty is not sent.
	Headers map[string]string `json:"headers,omitempty"`
	// Find, on a read, picks the object out of a list response: for APIs
	// that can list a resource but not get one (Snowflake roles).
	Find *Find `json:"find,omitempty"`
	// SendBody: send the spec as the body of this call too (a revoke; on
	// a delete, Grafana Asserts' thresholds, which a DELETE listing them
	// removes).
	SendBody bool `json:"sendBody,omitempty"`
	// BodyEnvelope, on an assoc's delete, wraps the undo's body instead of
	// the kind's BodyEnvelope: PagerDuty's change_tags takes {"add": [...]}
	// to make the link and {"remove": [...]} to undo it.
	BodyEnvelope string `json:"bodyEnvelope,omitempty"`
	// Outer holds fields sent at the top of this call's request body,
	// beside the kind's BodyEnvelope rather than inside it (Datadog's
	// sensitive data scanner takes {"data": {...}, "meta": {}} on every
	// write, and {"meta": {}} as the whole body of its DELETE).
	Outer map[string]any `json:"outer,omitempty"`
	// Query parameters sent with this call: Vault's UI headers answer all
	// their values only to a read with multivalue=true; a replace's
	// createMode=orReplace; Okta's user create, activate=false.
	//
	// A value that is a {field} template is filled from the spec as ID.From
	// is (Kind.FillQuery): Confluent Cloud reads and deletes a cluster only
	// with ?environment={environment.id}. A {current.field} template is
	// filled from the resource as the API last answered it (the read
	// before an update, status.atProvider before a delete): Grafana
	// Assistant authorizes a write by the scope the object has now, in an
	// X-Resource-Scope header. On an assoc's delete a {field} template is
	// the undone link's field (Kafka's DELETE .../acls?principal=...).
	Query map[string]string `json:"query,omitempty"`
	// Body holds fields merged into the request body for this operation
	// (JSON:API type when an update takes a different type than create,
	// such as Datadog's rum_application_update vs rum_application_create).
	// On a delete it is the whole body (inside the kind's envelope), and a
	// string value that is a {param} template is that path parameter or
	// the ID: Datadog deletes a synthetics suite with a POST naming it,
	// {"attributes": {"public_ids": ["{public_id}"]}}.
	Body map[string]any `json:"body,omitempty"`
	// OK lists HTTP statuses, besides 2xx (and 404, which is always gone),
	// that count as done for this call. Only a delete reads it: an API that
	// refuses to delete what goes only with its parent (Cloudflare answers
	// 409 to deleting a Worker's latest version or active deployment; both
	// are removed with the Worker) lets the object go and leaves the
	// resource to its parent, as Terraform does.
	OK []int `json:"ok,omitempty"`
	// GraphQL makes this call a GraphQL request to Path, the vendor's
	// GraphQL endpoint (graphql.go); every call of the kind is then one.
	GraphQL *GraphQL `json:"graphql,omitempty"`
	// Carry lists fields (dotted inside objects: metadata.resourceVersion)
	// this call sends as a read of the resource answers them just before,
	// else as the spec sets them: a write the API takes only with what it
	// last answered (a Kubernetes-style API's metadata.resourceVersion, a
	// GitHub file's blob sha), or with the name the server chose
	// (Grafana's time intervals). On an update or a delete.
	Carry []string `json:"carry,omitempty"`
	// Absent, on a read: the API answers 200, not 404, when there is no
	// resource, with these fields so (Atlas's push-based log export:
	// state: UNCONFIGURED); a null value means the field is missing or
	// empty (Atlas's search deployment answers {} with no id). Such an
	// answer counts as not found.
	Absent map[string]any `json:"absent,omitempty"`
	// Envelope is where this call's answer holds the object, instead of
	// the kind's Envelope, as a slash path whose numeric parts index an
	// array: Cloudflare's bulk creates answer {"result": [{...}]}
	// (result/0); a Cloudforce One asset is read as a one-element list.
	Envelope string `json:"envelope,omitempty"`
	// BodyArray: this call takes the object as a one-element JSON array,
	// inside the kind's BodyEnvelope (Cloudflare's bulk create of a secret
	// takes [{...}] while its update takes the object), as Kind.BodyArray
	// does for every call.
	BodyArray bool `json:"bodyArray,omitempty"`
	// Raw, on a read: the answer is not JSON but this spec field's value as
	// it is (Cloudflare's Workers KV answers a key's value as its bytes).
	Raw string `json:"raw,omitempty"`
	// BareBody: this call's Body is sent as it is, not inside the kind's
	// BodyEnvelope: an RPC API whose create and update take
	// {"collector": {...}} and whose delete takes {"id": ...} (Grafana
	// Fleet Management). A read's Body is always sent so.
	BareBody bool `json:"bareBody,omitempty"`
	// Request is this call's whole request body, built per call from the
	// spec, where each call takes the object in another shape (Datadog's
	// datastore item: a create of {"data": {"attributes": {"values":
	// ["{value}"]}}}, an update of {..., "item_changes": {"ops_set":
	// "{value}"}}). A string that is one {field} is that spec field as it
	// is (dotted for a nested one), a path parameter or the ID, or with
	// current. the resource as the API answered it last; text around
	// templates makes a string. A template with nothing to fill it leaves
	// its member out. It is sent as it is: not the spec's body, nor inside
	// the kind's BodyEnvelope (Outer still goes beside it). See request.go.
	Request map[string]any `json:"request,omitempty"`
	// Whole, on an update: the call sends the resource as a read just
	// before answers it, whole (outside the kind's envelope too), with each
	// of the body's fields in place of the read's at the kind's
	// BodyEnvelope: an API whose write replaces the resource, for a kind
	// that is one part of it (Okta's SAML app settings, settings.app of an
	// app written by PUT /api/v1/apps/{appId}, as Terraform does).
	Whole bool `json:"whole,omitempty"`
}

// ReadsByBody reports whether the kind's read sends a body (Op.Body on the
// read): one naming the object rather than its path (an RPC API's POST
// .../GetCollector {"id": ...}; Vault's POST .../secret-id-accessor/lookup
// {"secret_id_accessor": ...}), or a POST listing them all that the read
// finds it in (Grafana IRM's RolesService.GetRoles {}).
func (k *Kind) ReadsByBody() bool { return k.Read.Body != nil }

// IsAbsent reports whether a read's answer obs says there is no resource
// (Op.Absent).
func (op *Op) IsAbsent(obs map[string]any) bool {
	if op == nil || len(op.Absent) == 0 {
		return false
	}
	for f, want := range op.Absent {
		got := Dig(obs, strings.ReplaceAll(f, ".", "/"))
		if want == nil {
			if got != nil && got != "" {
				return false
			}
			continue
		}
		if got == nil || Text(got) != Text(want) {
			return false
		}
	}
	return true
}

// Allows reports whether status counts as done for this call (Op.OK).
func (op *Op) Allows(status int) bool {
	if op == nil {
		return false
	}
	for _, s := range op.OK {
		if s == status {
			return true
		}
	}
	return false
}

// Replace is how a CRUD kind with no update applies a changed spec: it
// creates the resource again in place (Snowflake's CREATE OR REPLACE, a POST
// to the collection with createMode=orReplace). The reconciler uses it only
// for objects that set spec.replaceOnChange: true.
type Replace struct {
	// Op.Query holds the parameters that make the create replace
	// (createMode=orReplace).
	Op `json:",inline"`
	// Identity lists the body fields that name the resource. Re-creating
	// with a different value would make a second resource and leave the
	// first, so a change to one is refused: delete and recreate instead.
	Identity []string `json:"identity,omitempty"`
}

// Find picks one element of a list response.
type Find struct {
	// List is the slash path to the array; "" means the response is one.
	List string `json:"list,omitempty"`
	// Field is the element field that must equal path parameter Param,
	// ignoring case; "" means the element itself, a bare string (Vault's
	// LIST answers {"keys": ["a", "b"]}), and the object found is then
	// empty.
	Field string `json:"field,omitempty"`
	Param string `json:"param,omitempty"`
	// Key, in place of Field and Param: the answer at List is not a list
	// but an object, keyed by this template of path parameters, whose
	// value is the object (Vault's secrets sync associations of a secret,
	// {"associated_destinations": {"gh/my-dest": {...}}}: "{type}/{name}").
	Key string `json:"key,omitempty"`
}

// KeyIn is the key a keyed find (Find.Key) looks for: its template filled
// from params; false where a parameter is missing.
func (f *Find) KeyIn(params map[string]string) (string, bool) {
	vals := make(map[string]any, len(params))
	for n, v := range params {
		vals[n] = v
	}
	return (&ID{From: f.Key}).Compose(vals)
}

// ListAt is where a find read's array is: Find.List, else the envelope a
// single answer would be in (Cloudflare's {"result": [...]}), else the
// answer itself.
func (k *Kind) ListAt() string {
	if k.Read.Find == nil {
		return ""
	}
	if k.Read.Find.List != "" {
		return k.Read.Find.List
	}
	return k.Envelope
}

// FindIn is the element of a find read's answer whose Find.Field (dotted
// for a nested one: user.id) is want, ignoring case. A find with neither
// field nor param finds the list's first element: a list of at most one,
// there or not (GitLab's runner controller's instance_level_scopings).
func (k *Kind) FindIn(resp any, want string) (map[string]any, bool) {
	if k.Read.Find.Key != "" {
		// Keyed: the object at want (Find.KeyIn) in the map there.
		byKey, _ := Dig(resp, k.ListAt()).(map[string]any)
		m, ok := byKey[want].(map[string]any)
		return m, ok
	}
	list := DigList(resp, k.ListAt())
	if fd := k.Read.Find; fd.Field == "" && fd.Param == "" {
		if len(list) == 0 {
			return nil, false
		}
		m, ok := list[0].(map[string]any)
		return m, ok
	}
	for _, e := range list {
		if k.Read.Find.Field == "" {
			if _, isObj := e.(map[string]any); !isObj && e != nil && strings.EqualFold(Text(e), want) {
				return map[string]any{}, true
			}
			continue
		}
		m, _ := e.(map[string]any)
		if v := Dig(m, strings.ReplaceAll(k.Read.Find.Field, ".", "/")); v != nil && strings.EqualFold(Text(v), want) {
			return m, true
		}
	}
	return nil, false
}

// Adopt is how a CRUD kind takes over a resource that exists before it,
// rather than making one (Okta's default authorization server and default
// policies): its create lists the resources and picks the one whose fields
// match; it is updated as usual, and deleting the object lets it go.
type Adopt struct {
	// Op is the list call (GET); Find.List says where its array is.
	Op `json:",inline"`
	// Match: element fields (dotted inside objects) and the values they
	// must have, ignoring case: a literal (system: "true"), or a {field}
	// template filled from the spec ({name}).
	Match map[string]string `json:"match"`
}

// Matches reports whether a list element e is the one Match describes,
// with templates filled from body.
func (a *Adopt) Matches(e map[string]any, body map[string]any) bool {
	for f, want := range a.Match {
		if IsTemplate(want) {
			v, ok := (&ID{From: want}).Compose(body)
			if !ok {
				return false
			}
			want = v
		}
		got := Dig(e, strings.ReplaceAll(f, ".", "/"))
		if got == nil || !strings.EqualFold(Text(got), want) {
			return false
		}
	}
	return true
}

// MatchesSpec is Matches where a {field} template whose field body lacks
// matches an element that lacks it too (or has it empty): the element
// holding just what the spec sets (a list item is an ip, an asn, a
// hostname or a redirect).
func (a *Adopt) MatchesSpec(e map[string]any, body map[string]any) bool {
	for f, want := range a.Match {
		got := Dig(e, strings.ReplaceAll(f, ".", "/"))
		if IsTemplate(want) {
			v, ok := (&ID{From: want}).Compose(body)
			if !ok {
				if got != nil && got != "" {
					return false
				}
				continue
			}
			want = v
		}
		if got == nil || !strings.EqualFold(Text(got), want) {
			return false
		}
	}
	return true
}

// Param fills one {param} of a path from a top-level spec field.
type Param struct {
	Name  string `json:"name"`
	Field string `json:"field"`
	// InBody: the field is a body field too, and is sent there as well as
	// in the path (Confluent's tag binding is deleted at
	// .../entity/type/{typeName}/name/{qualifiedName}/tags/{tagName}, the
	// create body's entityType, entityName and typeName).
	InBody bool `json:"inBody,omitempty"`
}

// In is the param's value in spec: its field, dotted for one nested in the
// body (Datadog's custom forecast is at .../budget/{budget_id}/..., its
// body's attributes.budget_uid); nil when the spec has none.
func (p Param) In(spec map[string]any) any {
	return Dig(spec, strings.ReplaceAll(p.Field, ".", "/"))
}

// ID says where a CRUD resource's server-assigned identifier comes from and
// which item-path parameter takes it.
type ID struct {
	Param string `json:"param"`
	// From is the field of the create response that holds the ID, as a
	// slash-separated path (e.g. "id", "result/id"); or a template that
	// composes it from the spec (see Compose), where the API names the item
	// by more than one field: Snowflake's function signature,
	// "{name}({arguments[].datatype})".
	From string `json:"from"`
	// Match, where the ID is part of the From field's value: a template
	// that value matches, whose {Param} is the ID (GitLab's epic board is
	// read over REST by the number inside the global ID its GraphQL create
	// answers: "gid://gitlab/Boards::EpicBoard/{id}").
	Match string `json:"match,omitempty"`
	// Find, where the create answers no ID (Cloudflare's list items are
	// added by an asynchronous bulk POST that answers an operation's ID):
	// the list the new resource is found in, by the element whose fields
	// Find.Match gives (MatchesSpec: a field the spec leaves out is one the
	// element lacks too). It is looked for before a create too, so that one
	// made by a create whose answer named nothing is not made again.
	Find *Adopt `json:"find,omitempty"`
}

// Part is the ID inside v, the From field's value: the part of it Match's
// {Param} stands for, or v itself where there is no Match. It fails where
// v does not match.
func (id *ID) Part(v string) (string, bool) {
	if id.Match == "" {
		return v, true
	}
	got, ok := MatchTemplate(id.Match, v)
	if !ok || got[id.Param] == "" {
		return "", false
	}
	return got[id.Param], true
}

// MatchTemplate matches s against a template of text and {name}s, each
// standing for at least one character, and returns what each stands for.
func MatchTemplate(tmpl, s string) (map[string]string, bool) {
	names := Placeholders(tmpl)
	parts := templateRE.Split(tmpl, -1)
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	m := regexp.MustCompile("^" + strings.Join(parts, "(.+?)") + "$").FindStringSubmatch(s)
	if m == nil {
		return nil, false
	}
	out := make(map[string]string, len(names))
	for i, n := range names {
		out[n] = m[i+1]
	}
	return out, true
}

// IsTemplate reports whether From composes the ID from the spec.
func (id *ID) IsTemplate() bool { return strings.Contains(id.From, "{") }

// Compose fills the From template from a body: {field} is the field's value
// and {list[].sub} the sub field of each element of list, comma-separated
// ("" for an empty or absent list). It fails when a {field} is absent.
func (id *ID) Compose(body map[string]any) (string, bool) {
	ok := true
	out := templateRE.ReplaceAllStringFunc(id.From, func(m string) string {
		ref := m[1 : len(m)-1]
		if list, sub, isList := strings.Cut(ref, "[]."); isList {
			elems, _ := body[list].([]any)
			parts := make([]string, 0, len(elems))
			for _, e := range elems {
				v := Dig(e, strings.ReplaceAll(sub, ".", "/"))
				if v == nil {
					ok = false
					return ""
				}
				parts = append(parts, Text(v))
			}
			return strings.Join(parts, ",")
		}
		var v any
		for _, alt := range strings.Split(ref, "|") {
			// {a|b}: the first of the fields that is set (a list item's
			// search term: its ip, else its asn, ...).
			if v = Dig(body, strings.ReplaceAll(alt, ".", "/")); v != nil && v != "" {
				break
			}
		}
		if v == nil || v == "" {
			ok = false
			return ""
		}
		return Text(v)
	})
	return out, ok
}

// Text is a JSON value as a path or an ID holds it: a whole number as an
// integer (12345678), not as fmt.Sprint writes a float64 (1.2345678e+07).
func Text(v any) string {
	if f, ok := v.(float64); ok && f == math.Trunc(f) && math.Abs(f) < 1<<53 {
		return strconv.FormatInt(int64(f), 10)
	}
	return fmt.Sprint(v)
}

var templateRE = regexp.MustCompile(`\{[^{}]+\}`)

// Kind is one generated kind.
type Kind struct {
	Kind    string `json:"kind"`
	Group   string `json:"group"`
	Version string `json:"version"`
	Plural  string `json:"plural"`
	Shape   Shape  `json:"shape"`

	Create *Op `json:"create,omitempty"`
	Read   Op  `json:"read"`
	Update *Op `json:"update,omitempty"`
	Delete *Op `json:"delete,omitempty"`
	// BeforeDelete calls are made in order, on the same path parameters,
	// before Delete: a resource the API will not delete until it is put in
	// some state first (GitLab answers 403 to deleting an environment that
	// is not stopped: POST .../environments/{id}/stop). A 404 is ignored.
	BeforeDelete []Op `json:"beforeDelete,omitempty"`
	// Replace, on a CRUD kind with no update, lets objects opt in to
	// applying a change by re-creating the resource in place.
	Replace *Replace `json:"replace,omitempty"`
	// Adopt, on a CRUD kind with no create, finds the resource that exists
	// already instead (Okta's default policies); such a kind has Forget.
	Adopt *Adopt `json:"adopt,omitempty"`

	Params []Param `json:"params,omitempty"`
	ID     *ID     `json:"id,omitempty"`

	// QueryFields are spec fields that are not sent in the body: they fill
	// {field} templates in calls' query parameters and headers (Grafana
	// Cloud's access policies take ?region= on every call; a Confluent
	// switchover pair, ?environment= on all but its create).
	QueryFields []string `json:"queryFields,omitempty"`
	// Body lists the spec fields sent as the request body, by the vendor
	// API's own names. Empty means every spec field that is not a path
	// parameter.
	Body []string `json:"body,omitempty"`
	// WriteOnly fields are sent but never read back (secrets, tokens);
	// they are not compared when looking for drift.
	WriteOnly []string `json:"writeOnly,omitempty"`
	// CreateOnly fields are sent when the resource is created and left out
	// of every update, which refuses them (PagerDuty's custom field
	// data_type: "doesn't match any valid key", or nested dotted paths such
	// as Datadog's "attributes.name"). A change to one is drift that only
	// re-creating the object applies.
	CreateOnly []string `json:"createOnly,omitempty"`
	// OmitUnchanged fields may be updated, but are left out of an update
	// when they already match the API (Confluent refuses a Kafka cluster
	// PATCH that carries config together with display_name).
	OmitUnchanged []string `json:"omitUnchanged,omitempty"`
	// Envelope is the field a response wraps the resource in, if any
	// (Cloudflare's "result", Datadog v2's "data"), as a slash path.
	Envelope string `json:"envelope,omitempty"`
	// BodyEnvelope wraps the request body the same way, if the API wants it.
	BodyEnvelope string `json:"bodyEnvelope,omitempty"`
	// BodyArray wraps a single-item request body in a one-element JSON array
	// ([body]) when sending it (Atlas's POST .../accessList takes
	// [{"ipAddress": ...}] while GET and DELETE act on one item).
	BodyArray bool `json:"bodyArray,omitempty"`
	// BodyField names the spec field whose array value is sent as the whole
	// request body (a bare JSON array) and where an unwrapped array response
	// is placed ({BodyField: array}): Cloudflare's CloudConnectorRules.
	BodyField string `json:"bodyField,omitempty"`
	// Multipart encodes Create and Update request bodies as
	// multipart/form-data instead of JSON (Cloudflare's WorkersScript and
	// Snippet).
	Multipart *Multipart `json:"multipart,omitempty"`
	// UpdateID is the body field an update names the resource's ID in, as
	// well as in its path: JSON:API's data.id (Datadog answers an update
	// without it 400 "Required parameter 'id' is missing").
	UpdateID string `json:"updateID,omitempty"`

	// CompareReturnedOnly: the spec does not describe what a read returns,
	// so a field the API does not send back is not drift (much of Vault).
	CompareReturnedOnly bool `json:"compareReturnedOnly,omitempty"`

	// CaseInsensitive fields are uppercased or lowercased by the API
	// (Snowflake identifiers); a change of case alone is not drift.
	CaseInsensitive []string `json:"caseInsensitive,omitempty"`

	// Compare names fields the API reads back in another form than it
	// takes, and the rule that says when the two are the same (see
	// CompareRules). GitLab takes an application's scopes as "api read_user"
	// and reads them back as ["api", "read_user"]: {scopes: splitSpace}. A
	// field is dotted inside objects, with [] for each element of a list
	// (GitLab's value stream stages take startEventLabelId and read back
	// startEventLabel {id}: {stages[].startEventLabelId: slugObject}).
	Compare map[string]string `json:"compare,omitempty"`

	// Terraform lists the Terraform resource types this kind stands in for.
	Terraform []string `json:"terraform,omitempty"`
	// Sample is a valid spec, used by tests and by the docs' example.
	Sample map[string]any `json:"sample,omitempty"`
	// ServerFields are fields the API's read answers but no request sends
	// (a timestamp, a URL, a status), with sample values: the fake answers
	// them beside what was sent, as the API would. Never part of the spec.
	ServerFields map[string]any `json:"serverFields,omitempty"`
	// Forget, a reason: the API cannot delete the resource (an export job,
	// an incident type), so the kind has no delete; deleting the object
	// lets go of the resource and leaves it in the API, as Terraform does.
	Forget string `json:"forget,omitempty"`
	// Unread, a reason: the kind never reads the resource, as Terraform's
	// provider never does, where the API has no read the spec can name
	// (Okta's resource owners are listed only by their parent's ORN, which
	// neither the spec nor the write names). A settings kind with no read:
	// its update is sent when the spec changes (status.appliedHash) and
	// never otherwise; nothing is compared, so a change made outside
	// Kubernetes is not seen; status.atProvider is what the last write
	// answered; deleting the object leaves the resource as it is.
	Unread string `json:"unread,omitempty"`
	// Stays, a reason: the delete call leaves the resource readable (it
	// closes a GitHub issue: PATCH {"state": "closed"}, delete.body), so
	// nothing checks it is gone.
	Stays string `json:"stays,omitempty"`
	// SecretOutputs are response fields (dotted inside objects) that hold
	// a secret the API makes (an API key's privateKey, a token's value),
	// often answered only on create. They are never kept in
	// status.atProvider; an object that sets spec.writeSecretRef.name gets
	// them in that Secret, which it owns.
	SecretOutputs []string `json:"secretOutputs,omitempty"`
	// Redact are fields (dotted inside objects; the last part is matched,
	// as for SecretOutputs) whose values are sensitive though the kind
	// keeps them (a Vault token's accessor, which is its ID): never written
	// to a cassette, a test log or a live record, wherever the value
	// appears (redacted as a secret output is, and then everywhere else).
	// Nothing else about the kind changes.
	Redact []string `json:"redact,omitempty"`
	// Rotate: the resource is a token that expires, made again before it
	// does (Terraform's *_rotating_token types). See rotate.go.
	Rotate *Rotate `json:"rotate,omitempty"`
	// Readiness: "fake" (proven against the fake API only), "live"
	// (proven against the real API at least once) or "draft" (a skeleton
	// the generator made for a Terraform type no kind covers: never
	// reconciled; see Draft).
	Readiness string `json:"readiness,omitempty"`
	// Draft, on a kind of readiness "draft": what is still open. Its
	// calls are the generator's best candidates, not a working kind's.
	Draft *Draft `json:"draft,omitempty"`
	// SQL, for shape sql: the statements that make, read, change and remove
	// the object through the SQL API at Read.Path.
	SQL *SQL `json:"sql,omitempty"`
	// Set, for shape set: the elements' field and key.
	Set *Set `json:"set,omitempty"`
	// Order, on a settings kind: the kind is the order of other objects,
	// read from a list and written whole by a reorder call (order.go).
	Order *Order `json:"order,omitempty"`
	// Element: the kind is one element of a list another resource holds,
	// read and written only as part of it (element.go).
	Element *Element `json:"element,omitempty"`
	// State: a field set by a call per value rather than in a body (a
	// lifecycle, a subscription), and what deleting the object sets it to
	// (element.go).
	State *State `json:"state,omitempty"`
}

// CompareSplitSpace: a string of space-separated words, read back as an
// array of those words in any order.
const CompareSplitSpace = "splitSpace"

// CompareEnabledObject: a value read back wrapped as {"enabled": value}
// (GitHub's branch protection takes enforce_admins: true and reads it back
// as {"url": ..., "enabled": true}).
const CompareEnabledObject = "enabledObject"

// CompareEndsWith: a string read back inside a longer one that ends with
// it, runs of white space counting as one space. Snowflake reads a view's
// query back as its whole CREATE VIEW statement.
const CompareEndsWith = "endsWith"

// CompareUnordered: a list read back in another order than written, each
// written element matching a different element read (PagerDuty's schedule
// layers come back highest priority first, the reverse of how they are sent).
const CompareUnordered = "unordered"

// CompareSlugObject: a slug or ID read back as the whole object it names,
// whose "slug" or "id" is that value (Sentry's cron monitor takes project:
// "my-project" and reads it back as {"id": "45...", "slug": "my-project", ...}).
const CompareSlugObject = "slugObject"

// CompareNodeIDs: a list of IDs read back as a GraphQL connection of the
// objects they name, {"nodes": [{"id": ...}, ...]}, in any order (GitLab
// takes a project's complianceFrameworkIds and reads back its
// complianceFrameworks { nodes { id } }).
const CompareNodeIDs = "nodeIDs"

// CompareSubsetByID: a list of objects read back holding more elements
// than written, in any order; each written element matches the element read
// with the same "id" (Cloudflare reads back every managed transform a zone
// has, each with its "enabled"; the spec lists those it sets).
const CompareSubsetByID = "subsetByID"

// CompareRules are the rules Kind.Compare may name.
var CompareRules = map[string]bool{CompareSplitSpace: true, CompareEnabledObject: true, CompareEndsWith: true, CompareUnordered: true, CompareSlugObject: true, CompareNodeIDs: true, CompareSubsetByID: true}

// ReadinessDraft marks a draft kind (Kind.Draft).
const ReadinessDraft = "draft"

// Draft is a draft kind's open items: a Terraform gap type the generator
// found no working kind for, with a spec from the Terraform provider's
// schema and the closest API paths as its calls. Nothing reconciles it: the
// manager runs no controller for it, the reconciler refuses it, the fake
// has no route for it, and the tests skip it. It becomes a kind once each
// open item is resolved in the generator's inputs (vendor.yaml, a
// supplement, a hand-written file), and goes away by itself then.
type Draft struct {
	// Open names each unresolved part: "delete: no DELETE on the item
	// path ...", "no API in spec: ...".
	Open []string `json:"open"`
	// Candidates are other API paths worth a look, with their methods.
	Candidates []string `json:"candidates,omitempty"`
}

// IsDraft reports whether the kind is a draft, never reconciled.
func (k *Kind) IsDraft() bool { return k.Readiness == ReadinessDraft || k.Draft != nil }

// DraftReason says why a draft kind is not run, for a test's skip or a
// refusal.
func (k *Kind) DraftReason() string {
	open := ""
	if k.Draft != nil {
		open = strings.Join(k.Draft.Open, "; ")
	}
	return fmt.Sprintf("%s.%s is a draft (never reconciled): %s", k.Kind, k.Group, open)
}

// Reconcilable is f without its draft kinds: the kinds a manager, the
// fake API and references may use.
func (f *File) Reconcilable() *File {
	out := *f
	out.Kinds = make([]Kind, 0, len(f.Kinds))
	for i := range f.Kinds {
		if !f.Kinds[i].IsDraft() {
			out.Kinds = append(out.Kinds, f.Kinds[i])
		}
	}
	return &out
}

// validateDraft checks a draft kind: marked as one, with what is open and
// the Terraform type it is for. Its calls are candidates and are not
// checked as a kind's are.
func (k *Kind) validateDraft() error {
	switch {
	case k.Readiness != ReadinessDraft || k.Draft == nil:
		return fmt.Errorf("a draft needs readiness %q and draft", ReadinessDraft)
	case len(k.Draft.Open) == 0:
		return fmt.Errorf("draft: nothing open: a kind, then, not a draft")
	case len(k.Terraform) == 0:
		return fmt.Errorf("draft: no Terraform type")
	case k.SQL != nil || k.Set != nil || k.Element != nil || k.State != nil || k.Order != nil:
		return fmt.Errorf("draft: only calls, params and a sample")
	}
	return nil
}

// GVK of the kind.
func (k *Kind) GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: k.Group, Version: k.Version, Kind: k.Kind}
}

// File is a vendor's types.yaml.
type File struct {
	Vendor     string `json:"vendor"`
	Connection struct {
		Group   string `json:"group"`
		Version string `json:"version"`
		Kind    string `json:"kind"`
	} `json:"connection"`
	Kinds []Kind `json:"kinds"`
}

// ConnectionGVK of the vendor's connection kind.
func (f *File) ConnectionGVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: f.Connection.Group, Version: f.Connection.Version, Kind: f.Connection.Kind}
}

// Parse reads and checks a types.yaml.
func Parse(data []byte) (*File, error) {
	var f File
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i := range f.Kinds {
		k := &f.Kinds[i]
		key := k.GVK().String()
		if seen[key] {
			return nil, fmt.Errorf("%s: duplicate kind", key)
		}
		seen[key] = true
		if err := k.validate(); err != nil {
			return nil, fmt.Errorf("%s: %w", key, err)
		}
	}
	return &f, nil
}

// placeholder is a path's {param}, or {+param}: a value that keeps its
// slashes (RFC 6570's reserved expansion: GitHub's branch "feature/x" in
// .../git/refs/heads/{+branch}).
var placeholder = regexp.MustCompile(`\{\+?([^}]+)\}`)

// Placeholders returns the {param} names of a path, in order ({+param}'s
// name is param).
func Placeholders(path string) []string {
	var out []string
	for _, m := range placeholder.FindAllStringSubmatch(path, -1) {
		out = append(out, m[1])
	}
	return out
}

func (k *Kind) validate() error {
	if k.IsDraft() {
		return k.validateDraft()
	}
	if k.Shape == SQLShape || k.SQL != nil {
		if k.Shape != SQLShape {
			return fmt.Errorf("sql is for shape sql")
		}
		return k.validateSQL()
	}
	if k.Shape == SetShape || k.Set != nil {
		return k.validateSet()
	}
	if k.Unread != "" {
		switch {
		case k.Shape != Settings || k.Update == nil:
			return fmt.Errorf("unread is for a settings kind with an update")
		case k.Read.Method != "" || k.Read.Path != "":
			return fmt.Errorf("unread: the kind has no read")
		case k.Create != nil || k.Delete != nil || k.State != nil || k.Order != nil || k.Element != nil || k.Update.Whole || len(k.Update.Carry) > 0 || k.HasCurrent() || k.Update.GraphQL != nil:
			return fmt.Errorf("unread: an update only (no create, delete, state, order, element, whole, carry, {current.} or GraphQL): nothing is read")
		}
	}
	switch k.Shape {
	case CRUD:
		if (k.Create == nil) == (k.Adopt == nil) || (k.Delete == nil && k.Forget == "" && !k.StateDeletes()) || k.ID == nil {
			return fmt.Errorf("crud needs create (or adopt: how to find it), delete (or forget: why not, or state.delete) and id")
		}
		if k.Adopt != nil && (k.Update == nil || len(k.Adopt.Match) == 0) {
			return fmt.Errorf("adopt needs an update and match")
		}
		if k.Delete != nil && k.Forget != "" {
			return fmt.Errorf("forget is for a kind with no delete")
		}
	case Upsert, Settings, Assoc:
		if k.Forget != "" {
			return fmt.Errorf("forget is for crud kinds")
		}
	}
	switch k.Shape {
	case CRUD:
	case Upsert:
		// With a create and no update: a link the caller names, which a
		// POST makes and a DELETE removes, with nothing to change (GitLab's
		// runner controller scopes; Vault's generated KMIP CA).
		if (k.Update == nil && k.Create == nil) || k.Delete == nil {
			return fmt.Errorf("upsert needs update (the PUT) or a create, and delete")
		}
	case Settings:
		if k.Update == nil && k.State == nil {
			return fmt.Errorf("settings needs update (or state)")
		}
	case Assoc:
		if k.Create == nil || k.Delete == nil {
			return fmt.Errorf("assoc needs create and delete")
		}
	default:
		return fmt.Errorf("unknown shape %q", k.Shape)
	}
	if k.IsGraphQL() {
		if err := k.validateGraphQL(); err != nil {
			return err
		}
	}
	if k.ID != nil && k.ID.Find != nil && (k.Shape != CRUD || k.Create == nil || k.ID.Find.Method != "GET" || len(k.ID.Find.Match) == 0) {
		return fmt.Errorf("id.find is for a crud kind's create: a GET of the list and match")
	}
	if k.ID != nil && k.ID.Match != "" && !strings.Contains(k.ID.Match, "{"+k.ID.Param+"}") {
		return fmt.Errorf("id.match %q has no {%s}", k.ID.Match, k.ID.Param)
	}
	if k.Element != nil {
		if err := k.Element.validate(k); err != nil {
			return err
		}
	}
	if k.State != nil {
		if err := k.State.validate(k); err != nil {
			return err
		}
	}
	if k.Replace != nil && (k.Shape != CRUD || k.Update != nil) {
		return fmt.Errorf("replace is for crud kinds with no update")
	}
	if len(k.BeforeDelete) > 0 && (k.Delete == nil || k.Shape == Assoc) {
		return fmt.Errorf("beforeDelete needs a delete, and is not for assoc kinds")
	}
	if len(k.CreateOnly) > 0 && (k.Shape != CRUD && k.Shape != Upsert || k.Update == nil) {
		return fmt.Errorf("createOnly is for crud and upsert kinds with an update")
	}
	if k.Adopt != nil && k.Shape != CRUD {
		return fmt.Errorf("adopt is for crud kinds")
	}
	if k.Read.Carry != nil || (k.Create != nil && k.Create.Carry != nil) {
		return fmt.Errorf("carry is for an update or a delete")
	}
	for _, op := range []*Op{&k.Read, k.Create, k.Delete} {
		if op != nil && op.Whole {
			return fmt.Errorf("whole is for an update")
		}
	}
	if u := k.Update; u != nil && u.Whole && (u.GraphQL != nil || u.Request != nil || k.Multipart != nil || k.Order != nil || k.BodyField != "" || k.BodyArray || u.BodyArray || len(u.Outer) > 0 || k.Element != nil || k.Read.Raw != "" || k.Read.Find != nil || k.Read.Body != nil) {
		return fmt.Errorf("update.whole sends the resource a plain read answers, with the body in place: not for GraphQL, a per-call request, multipart, order, element, outer, array bodies, or a read that finds, sends a body or is raw")
	}
	if len(k.OmitUnchanged) > 0 && (k.Shape != CRUD || k.Update == nil) {
		return fmt.Errorf("omitUnchanged is for crud kinds with an update")
	}
	if k.BodyArray && k.BodyField != "" {
		return fmt.Errorf("bodyArray and bodyField cannot both be set")
	}
	if k.BodyField != "" && len(k.Body) > 0 {
		return fmt.Errorf("bodyField and body cannot both be set")
	}
	if k.Delete != nil && k.Delete.SendBody && k.Delete.Body != nil {
		return fmt.Errorf("delete sends the spec (sendBody) or a fixed body, not both")
	}
	for _, op := range k.ops() {
		if op.BodyEnvelope != "" && (k.Shape != Assoc || op != k.Delete) {
			return fmt.Errorf("%s %s: bodyEnvelope on a call is for an assoc's delete", op.Method, op.Path)
		}
		if op.BodyArray && (k.BodyArray || k.BodyField != "" || k.Multipart != nil) {
			return fmt.Errorf("%s %s: bodyArray on a call is for a kind whose other calls send the object as it is", op.Method, op.Path)
		}
		if op.Raw != "" && op != &k.Read {
			return fmt.Errorf("%s %s: raw is for a read", op.Method, op.Path)
		}
		if err := k.validateRequest(op); err != nil {
			return err
		}
	}
	if k.Multipart != nil {
		for _, c := range k.Multipart.Calls {
			if (c != "create" || k.Create == nil) && (c != "update" || k.Update == nil) {
				return fmt.Errorf("multipart.calls: %q is not a call of the kind (create, update)", c)
			}
		}
	}
	if k.Order != nil {
		if err := k.validateOrder(); err != nil {
			return err
		}
	}
	if k.Rotate != nil {
		if err := k.validateRotate(); err != nil {
			return err
		}
	}
	for _, p := range k.Params {
		if strings.Contains(p.Field, ".") && !p.InBody {
			return fmt.Errorf("params: %s: a nested field fills a path parameter only as a body field (inBody)", p.Field)
		}
	}
	for _, f := range k.QueryFields {
		for _, p := range k.Params {
			if p.Field == f {
				return fmt.Errorf("queryFields.%s fills a path parameter too", f)
			}
		}
	}
	for f, rule := range k.Compare {
		if !CompareRules[rule] {
			return fmt.Errorf("compare.%s: unknown rule %q", f, rule)
		}
		if strings.Contains(f, "/") || strings.Contains(strings.ReplaceAll(f, "[].", "."), "[]") || strings.Contains(f, "..") || strings.HasPrefix(f, ".") {
			return fmt.Errorf("compare.%s: a field, dotted inside objects, [] for each element of a list (stages[].startEventLabelId)", f)
		}
	}
	known := map[string]bool{}
	for _, p := range k.Params {
		known[p.Name] = true
	}
	if k.ID != nil {
		known[k.ID.Param] = true
	}
	if k.Read.Body != nil {
		if k.Read.Method == "GET" || k.Read.Raw != "" || k.IsGraphQL() || (k.Shape != CRUD && k.Shape != Upsert) {
			return fmt.Errorf("read.body: a crud or upsert kind's read that sends a body (POST), not a GET, a raw read or GraphQL")
		}
		if k.Shape == CRUD && k.Read.Find == nil && !strings.Contains(fmt.Sprint(k.Read.Body), "{"+k.ID.Param+"}") {
			return fmt.Errorf("read.body: names no {%s}, and finds nothing", k.ID.Param)
		}
	}
	if fd := k.Read.Find; fd != nil && fd.Key != "" {
		if fd.Field != "" || fd.Param != "" {
			return fmt.Errorf("read.find: key is in place of field and param")
		}
		for _, name := range Placeholders(fd.Key) {
			if !known[name] {
				return fmt.Errorf("read.find.key %s: no param fills {%s}", fd.Key, name)
			}
		}
	}
	ops := []*Op{k.Create, &k.Read, k.Update, k.Delete}
	if k.Replace != nil {
		ops = append(ops, &k.Replace.Op)
	}
	if k.Adopt != nil {
		ops = append(ops, &k.Adopt.Op)
	}
	if k.ID != nil && k.ID.Find != nil {
		ops = append(ops, &k.ID.Find.Op)
	}
	for i := range k.BeforeDelete {
		ops = append(ops, &k.BeforeDelete[i])
	}
	ops = append(ops, k.stateOps()...)
	for _, op := range ops {
		if op == nil {
			continue
		}
		for _, name := range Placeholders(op.Path) {
			if strings.HasPrefix(name, "current.") && op == k.Delete && k.Shape != Assoc {
				continue // filled from the resource as last read (FillCurrent)
			}
			if !known[name] {
				return fmt.Errorf("%s %s: no param fills {%s}", op.Method, op.Path, name)
			}
		}
	}
	return nil
}

// validateSet checks a kind of shape set: a list read, an element create
// and delete on paths whose other placeholders are params, and nothing the
// set's calls do not use.
func (k *Kind) validateSet() error {
	st := k.Set
	switch {
	case k.Shape != SetShape:
		return fmt.Errorf("set is for shape set")
	case st == nil || st.Field == "":
		return fmt.Errorf("set needs set.field, and set.key and set.param (or set.keys)")
	case len(st.Keys) == 0 && (st.Key == "" || st.Param == ""):
		return fmt.Errorf("set needs set.key and set.param, or set.keys")
	case len(st.Keys) > 0 && (st.Key != "" || st.Param != "" || st.KeyURL != "" || st.ReadEach != nil):
		return fmt.Errorf("set.keys is in place of set.key and set.param, and not with keyURL or readEach")
	case k.Create == nil || k.Delete == nil:
		return fmt.Errorf("set needs create (add one element) and delete (remove one)")
	case k.IsGraphQL() || k.ID != nil || k.Read.Find != nil || k.Replace != nil || len(k.BeforeDelete) > 0 || k.Forget != "" || k.Stays != "":
		return fmt.Errorf("set: REST calls only, and no id, find, replace, beforeDelete, forget or stays")
	case k.Envelope != "" || k.BodyEnvelope != "" || k.BodyArray || k.BodyField != "" || k.Multipart != nil || k.UpdateID != "" || len(k.Body) > 0:
		return fmt.Errorf("set: an element is sent as it is: no envelope, bodyEnvelope, bodyArray, bodyField, multipart, updateID or body")
	}
	params := map[string]bool{}
	for _, p := range k.Params {
		params[p.Name] = true
		if p.Field == st.Field {
			return fmt.Errorf("set.field %s fills a path parameter", st.Field)
		}
	}
	check := []*Op{&k.Read}
	if h := st.Holder; h != nil {
		check = append(check, &h.Create, &h.Read, &h.Delete)
		for _, f := range h.Fields {
			if f == st.Field {
				return fmt.Errorf("set.holder.fields: %s is the elements' field", f)
			}
		}
	}
	if b := st.Batch; b != nil {
		switch {
		case b.Field == "" || b.Key == "" || b.At == "" || b.Value == "" || b.Add.Method == "" || b.Clear.Method == "":
			return fmt.Errorf("set.batch needs field, key, at, value, add and clear")
		case b.Field == st.Field:
			return fmt.Errorf("set.batch.field %s is the elements' field", b.Field)
		case b.Add.GraphQL != nil || b.Clear.GraphQL != nil:
			return fmt.Errorf("set.batch: REST calls only")
		}
		check = append(check, &b.Add, &b.Clear)
		for _, p := range k.Params {
			if p.Field == b.Field {
				return fmt.Errorf("set.batch.field %s fills a path parameter", b.Field)
			}
		}
	}
	for _, op := range check {
		for _, name := range Placeholders(op.Path) {
			if !params[name] {
				return fmt.Errorf("%s %s: no param fills {%s}", op.Method, op.Path, name)
			}
		}
	}
	for _, op := range []*Op{k.Create, k.Update, k.Delete, st.ReadEach} {
		if op == nil {
			continue
		}
		if op.GraphQL != nil {
			return fmt.Errorf("set: REST calls only")
		}
		if op == k.Create {
			continue
		}
		if len(st.Keys) == 0 {
			if !strings.Contains(op.Path, "{"+st.Param+"}") && !strings.Contains(op.Path, "{+"+st.Param+"}") {
				return fmt.Errorf("%s %s: an element call names the element by {%s}", op.Method, op.Path, st.Param)
			}
			continue
		}
		// Named by its keys, in its path or its query.
		named := map[string]bool{}
		for _, n := range Placeholders(op.Path) {
			named[n] = true
		}
		for _, v := range op.Query {
			for _, n := range Placeholders(v) {
				named[n] = true
			}
		}
		for _, f := range st.Keys {
			if !named[f] {
				return fmt.Errorf("%s %s: an element call names the element by each of set.keys: no {%s}", op.Method, op.Path, f)
			}
		}
	}
	return nil
}

// ReadsElsewhere reports whether the kind reads its resource as one object
// on another path than it writes it at (no find): Grafana's public
// dashboard is read at .../public-dashboards and updated and deleted at
// .../public-dashboards/{uid}; Vault's tokenization key configuration is
// written at .../keys/{name}/config and read at .../keys/{name}. A kind
// only deleted elsewhere (Grafana's DELETE /admin/users/{id}) does not.
func (k *Kind) ReadsElsewhere() bool {
	if k.Read.Find != nil || k.Shape == Assoc || k.Order != nil {
		return false
	}
	has := map[string]bool{}
	for _, p := range Placeholders(k.Read.Path) {
		has[p] = true
	}
	if k.Update != nil && k.Update.Path != k.Read.Path && k.UpdateID == "" {
		// Written at a path of its own that names all the read's does
		// (.../keys/{name}/config); not an update elsewhere that names the
		// item in its body (UpdateID: Grafana SM's POST /check/update).
		all := true
		names := map[string]bool{}
		for _, p := range Placeholders(k.Update.Path) {
			names[p] = true
		}
		for p := range has {
			all = all && names[p]
		}
		if all {
			return true
		}
	}
	for _, op := range []*Op{k.Update, k.Delete} {
		if op == nil {
			continue
		}
		for _, p := range Placeholders(op.Path) {
			if !has[p] && !strings.HasPrefix(p, "current.") {
				return true
			}
		}
	}
	return false
}

// ItemPath is the path the resource itself is at, which a fake API keeps it
// under: the read path, or for a kind that reads elsewhere, the delete's
// path (else the update's).
func (k *Kind) ItemPath() string {
	if k.Unread != "" {
		return k.Update.Path // never read: where it is written
	}
	if !k.ReadsElsewhere() {
		return k.Read.Path
	}
	if k.Delete != nil && (k.Update == nil || len(Placeholders(k.Delete.Path)) >= len(Placeholders(k.Update.Path))) {
		return k.Delete.Path
	}
	return k.Update.Path
}

// ParamField returns the spec field that fills a path parameter.
func (k *Kind) ParamField(name string) (string, bool) {
	for _, p := range k.Params {
		if p.Name == name {
			return p.Field, true
		}
	}
	return "", false
}

// IsWriteOnly reports whether a body field is never read back.
func (k *Kind) IsWriteOnly(field string) bool {
	for _, w := range k.WriteOnly {
		if w == field {
			return true
		}
	}
	return false
}

// FillQuery fills each {field} template among the query parameters and
// headers of the kind's calls from the spec, into params, keyed by the
// template itself; Op.QueryOf and Op.HeadersOf read them back. A template
// whose field the spec lacks fails. {current.field} templates are left to
// FillCurrent.
func (k *Kind) FillQuery(spec map[string]any, params map[string]string) error {
	for _, op := range k.ops() {
		for _, v := range op.templates() {
			if IsCurrent(v) || (op != &k.Read && k.Set.namesElement(v)) {
				continue // an element's key, filled per element (Set.Keys)
			}
			got, ok := (&ID{From: v}).Compose(spec)
			if !ok && k.namesParams(v) {
				continue // a path parameter or the ID ({id}): filled when sent (fromParams)
			}
			if !ok {
				return fmt.Errorf("spec.%s is required (%s)", strings.Trim(templateRE.FindString(v), "{}"), v)
			}
			params[v] = got
		}
	}
	return nil
}

// namesParams reports whether every placeholder of template v is a path
// parameter of the kind or its ID: a read narrowed to the resource it
// reads (Datadog's ?filter[connection_ids]={id}).
func (k *Kind) namesParams(v string) bool {
	for _, n := range Placeholders(v) {
		if !k.IsParam(n) {
			return false
		}
	}
	return true
}

// fromParams fills template v from the path parameters and the ID in
// params, where Kind.FillQuery left it to them; "" when one is missing.
func fromParams(v string, params map[string]string) string {
	vals := make(map[string]any, len(params))
	for n, x := range params {
		if !IsTemplate(n) {
			vals[n] = x
		}
	}
	got, _ := (&ID{From: v}).Compose(vals)
	return got
}

// FillCurrent fills {current.field} templates among the kind's query
// parameters and headers from cur, the resource as the API last answered
// it, into params (as FillQuery does). A field cur lacks leaves the
// template unfilled: an empty value. A delete path's {current.field}
// placeholder (Grafana k6's schedule, deleted at /schedules/{current.id})
// is filled under its own name, as a path parameter.
func (k *Kind) FillCurrent(cur map[string]any, params map[string]string) {
	if k.Delete != nil {
		for _, name := range Placeholders(k.Delete.Path) {
			if !strings.HasPrefix(name, "current.") {
				continue
			}
			if v := Dig(cur, strings.ReplaceAll(strings.TrimPrefix(name, "current."), ".", "/")); v != nil && Text(v) != "" {
				params[name] = Text(v)
			} else {
				delete(params, name)
			}
		}
	}
	for _, op := range k.ops() {
		for _, v := range op.templates() {
			if !IsCurrent(v) {
				continue
			}
			if got, ok := (&ID{From: v}).Compose(map[string]any{"current": cur}); ok {
				params[v] = got
			} else {
				delete(params, v)
			}
		}
	}
}

// HasCurrent reports whether a call of the kind has a {current.field}
// template (FillCurrent).
func (k *Kind) HasCurrent() bool {
	if k.Delete != nil && strings.Contains(k.Delete.Path, "{current.") {
		return true
	}
	for _, op := range k.ops() {
		for _, v := range op.templates() {
			if IsCurrent(v) {
				return true
			}
		}
	}
	return false
}

// ops is every call the kind makes.
func (k *Kind) ops() []*Op {
	ops := []*Op{k.Create, &k.Read, k.Update, k.Delete}
	for i := range k.BeforeDelete {
		ops = append(ops, &k.BeforeDelete[i])
	}
	if k.Replace != nil {
		ops = append(ops, &k.Replace.Op)
	}
	ops = append(ops, k.stateOps()...)
	out := ops[:0]
	for _, op := range ops {
		if op != nil {
			out = append(out, op)
		}
	}
	return out
}

// templates are the call's query and header values that hold a template.
func (op *Op) templates() []string {
	var out []string
	for _, m := range []map[string]string{op.Query, op.Headers} {
		for _, v := range m {
			if IsTemplate(v) {
				out = append(out, v)
			}
		}
	}
	return out
}

// IsTemplate reports whether a query value is a {field} template.
func IsTemplate(v string) bool { return templateRE.MatchString(v) }

// IsCurrent reports whether a value is filled from the resource as the API
// last answered it ({current.scope}), not from the spec.
func IsCurrent(v string) bool { return strings.Contains(v, "{current.") }

// QueryOf is the call's query parameters, with templates filled from params
// (Kind.FillQuery).
func (op *Op) QueryOf(params map[string]string) map[string]string {
	if op == nil || len(op.Query) == 0 {
		return nil
	}
	out := make(map[string]string, len(op.Query))
	for q, v := range op.Query {
		if IsTemplate(v) {
			got, ok := params[v]
			if !ok {
				got = fromParams(v, params)
			}
			v = got
		}
		out[q] = v
	}
	return out
}

// HeadersOf is the call's headers, with templates filled from params as
// QueryOf fills its query; a header whose template is empty is left out.
func (op *Op) HeadersOf(params map[string]string) map[string]string {
	if op == nil || len(op.Headers) == 0 {
		return nil
	}
	out := make(map[string]string, len(op.Headers))
	for h, v := range op.Headers {
		if IsTemplate(v) {
			got, ok := params[v]
			if !ok {
				got = fromParams(v, params)
			}
			if v = got; v == "" {
				continue
			}
		}
		out[h] = v
	}
	return out
}

// QueryFrom is the call's query parameters with templates filled from body
// (an assoc's undone link), else from params.
func (op *Op) QueryFrom(body map[string]any, params map[string]string) map[string]string {
	out := op.QueryOf(params)
	for q, v := range op.Query {
		if IsTemplate(v) {
			if got, ok := (&ID{From: v}).Compose(body); ok {
				out[q] = got
			}
		}
	}
	return out
}

// IsQueryField reports whether a spec field fills only query or header
// templates, and is not sent in the body.
func (k *Kind) IsQueryField(field string) bool { return containsField(k.QueryFields, field) }

// IsCreateOnly reports whether a body field is sent on create only.
func (k *Kind) IsCreateOnly(field string) bool {
	return containsField(k.CreateOnly, field)
}

// Multipart configures multipart/form-data encoding for a kind's request bodies.
type Multipart struct {
	// Files lists the top-level body fields that hold uploaded file(s)
	// ({name, content, contentBase64, contentType}, or a slice of them).
	Files []string `json:"files,omitempty"`
	// Calls are the calls whose bodies are multipart ("create",
	// "update"); the others send JSON. Empty: both (Grafana k6 makes a
	// load test with a multipart upload and changes it with a JSON PATCH).
	Calls []string `json:"calls,omitempty"`
}

// MultipartOn reports whether op's request body is multipart/form-data.
func (k *Kind) MultipartOn(op *Op) bool {
	if k.Multipart == nil {
		return false
	}
	if len(k.Multipart.Calls) == 0 || op == nil {
		return true
	}
	return op == k.Create && containsField(k.Multipart.Calls, "create") || op == k.Update && containsField(k.Multipart.Calls, "update")
}

// IsFile reports whether field is a file upload part.
func (m *Multipart) IsFile(field string) bool {
	return m != nil && containsField(m.Files, field)
}

// IsOmitUnchanged reports whether a body field is omitted from updates when unchanged.
func (k *Kind) IsOmitUnchanged(field string) bool {
	return containsField(k.OmitUnchanged, field)
}

func containsField(fields []string, field string) bool {
	for _, f := range fields {
		if f == field {
			return true
		}
	}
	return false
}

// DigList is the array at a slash path, where a part that is "*" stands
// for each element of the array there, their arrays joined in order: a
// list kept two levels down (Datadog's EventBridge sources, each account's
// event_hubs: data/attributes/accounts/*/event_hubs).
func DigList(v any, path string) []any {
	before, after, many := strings.Cut(path, "/*")
	if !many {
		l, _ := Dig(v, path).([]any)
		return l
	}
	outer, _ := Dig(v, before).([]any)
	var out []any
	for _, e := range outer {
		out = append(out, DigList(e, strings.TrimPrefix(after, "/"))...)
	}
	return out
}

// Dig walks a slash path into a decoded JSON object; a part that is a
// number indexes an array (result/0: the first element of result).
func Dig(v any, path string) any {
	if path == "" {
		return v
	}
	for _, part := range strings.Split(path, "/") {
		if l, ok := v.([]any); ok {
			i, err := strconv.Atoi(part)
			if err != nil || i < 0 || i >= len(l) {
				return nil
			}
			v = l[i]
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[part]
	}
	return v
}

// Wrap puts v at a slash path, as Dig finds it: each part an object's
// field, or for a number an array holding v at that index (result/0: v as
// {"result": [v]}).
func Wrap(path string, v any) any {
	if path == "" {
		return v
	}
	parts := strings.Split(path, "/")
	for i := len(parts) - 1; i >= 0; i-- {
		if n, err := strconv.Atoi(parts[i]); err == nil && n >= 0 && n < 1000 {
			l := make([]any, n+1)
			l[n] = v
			v = l
			continue
		}
		v = map[string]any{parts[i]: v}
	}
	return v
}

// WithoutFields returns a copy of body with each dotted field path removed,
// without mutating body or any nested map inside it.
func WithoutFields(body map[string]any, paths []string) map[string]any {
	if len(paths) == 0 || body == nil {
		return body
	}
	out := cloneMap(body)
	for _, p := range paths {
		deleteDotted(out, strings.Split(p, "."))
	}
	return out
}

func deleteDotted(m map[string]any, parts []string) {
	if len(parts) == 0 || m == nil {
		return
	}
	if len(parts) == 1 {
		delete(m, parts[0])
		return
	}
	sub, ok := m[parts[0]].(map[string]any)
	if !ok {
		return
	}
	sub = cloneMap(sub)
	m[parts[0]] = sub
	deleteDotted(sub, parts[1:])
}

func cloneMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// PickField returns a nested map containing only the dotted path from body,
// or (nil, false) if any segment along the path is absent.
func PickField(body map[string]any, path string) (map[string]any, bool) {
	parts := strings.Split(path, ".")
	var cur any = body
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, exists := m[p]
		if !exists {
			return nil, false
		}
		cur = v
	}
	res := cur
	for i := len(parts) - 1; i >= 0; i-- {
		res = map[string]any{parts[i]: res}
	}
	out, _ := res.(map[string]any)
	return out, true
}
