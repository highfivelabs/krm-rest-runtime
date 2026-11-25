// Copyright 2026 High Five Labs
// SPDX-License-Identifier: Apache-2.0

// Command rescrub redacts committed cassettes again by today's rules, with
// no live run: each value redaction takes out whole (a secret under its
// name, a kind's secretOutputs and redact fields, a secret's value written
// after its name in an error) is redacted wherever else it is, in every
// cassette of the directory, as a live run now records it. Placeholders,
// identifiers ({{id:N}}) and everything else stay as they are, so the
// cassettes replay as before.
//
// Run from a vendor repository's root:
//
//	go run github.com/highfivelabs/krm-rest-runtime/cmd/rescrub [-types types.yaml] [-live hack/live/live.yaml] [-check] [-redact-env VAR,...] [-redact-stdin] [dir ...]
//
// dir defaults to hack/live/cassettes (and hack/fake/cassettes, if it is
// there). The cassettes of a live run's directory share what each one's
// redaction learns, as the run did; a rehearsal's (a directory under
// hack/fake) do not, as the rehearsal did not: the fake answers the same
// sample values to every kind. -check writes nothing, lists what would change and exits 1 if
// anything would.
//
// Identities (values that name the test account or a person: a login, an
// account's subdomain) are redacted too, wherever they are: those live.yaml
// names (redactVars, redactFields, redactSubdomains, and vars saved from a
// login, username, handle or email field), found in the live cassettes as a
// live run finds them; and the values given in the environment variables
// -redact-env names (one value per line) or on standard input
// (-redact-stdin, one per line), in every directory. Never from a file: a
// value to redact must not be written down where it could be committed.
// What the live cassettes teach is redacted in hack/live/records.json
// (-records) too.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/highfivelabs/krm-rest-runtime/cassette"
	"github.com/highfivelabs/krm-rest-runtime/descriptor"
)

func main() {
	types := flag.String("types", "types.yaml", "the vendor's types.yaml: each kind's secretOutputs and redact fields")
	check := flag.Bool("check", false, "write nothing; list what would change and exit 1 if anything would")
	live := flag.String("live", filepath.Join("hack", "live", "live.yaml"), "the vendor's live.yaml: the identities it names (redactVars, redactFields, redactSubdomains)")
	records := flag.String("records", filepath.Join("hack", "live", "records.json"), "the live records, redacted with what the live cassettes teach")
	redactEnv := flag.String("redact-env", "", "comma-separated environment variables, each holding values to redact (one per line)")
	redactStdin := flag.Bool("redact-stdin", false, "read values to redact from standard input, one per line")
	flag.Parse()
	dirs := flag.Args()
	if len(dirs) == 0 {
		dirs = []string{filepath.Join("hack", "live", "cassettes")}
		if st, err := os.Stat(filepath.Join("hack", "fake", "cassettes")); err == nil && st.IsDir() {
			dirs = append(dirs, filepath.Join("hack", "fake", "cassettes"))
		}
	}
	fields, err := kindFields(*types)
	if err != nil {
		fail(err)
	}
	ids, err := liveIdentities(*live)
	if err != nil {
		fail(err)
	}
	var stdin io.Reader
	if *redactStdin {
		stdin = os.Stdin
	}
	vals, err := givenValues(*redactEnv, os.Getenv, stdin)
	if err != nil {
		fail(err)
	}
	changed := 0
	var liveKnown *cassette.Known
	for _, dir := range dirs {
		shared := !strings.Contains(filepath.ToSlash(dir)+"/", "hack/fake/")
		mk := func() *cassette.Known {
			k := cassette.NewKnown()
			if shared {
				k.Identify(ids) // a rehearsal finds none: the fake's values name no one
			}
			k.AddIdentity(vals...)
			return k
		}
		n, known, err := rescrub(dir, fields, shared, mk, *check, os.Stdout)
		if err != nil {
			fail(err)
		}
		changed += n
		if shared && liveKnown == nil {
			liveKnown = known
		}
	}
	if liveKnown == nil {
		liveKnown = cassette.NewKnown()
		liveKnown.AddIdentity(vals...)
	}
	n, err := redactRecords(*records, liveKnown, *check, os.Stdout)
	if err != nil {
		fail(err)
	}
	changed += n
	if *check && changed > 0 {
		os.Exit(1)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "rescrub:", err)
	os.Exit(2)
}

// kindFields: each kind's cassette name (<Kind>.<group>) and the fields its
// cassette redacts by name. No types.yaml: none.
func kindFields(path string) (map[string][]string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	f, err := descriptor.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string][]string{}
	for _, k := range f.Kinds {
		if fs := append(append([]string(nil), k.SecretOutputs...), k.Redact...); len(fs) > 0 {
			out[k.Kind+"."+k.Group] = fs
		}
	}
	return out, nil
}

// liveYAML is the part of a live.yaml rescrub reads: the identities it
// names, and the calls that save vars (one saved from a person's login is
// an identity too).
type liveYAML struct {
	cassette.Identities
	Before []liveCall `json:"before"`
	Kinds  []struct {
		Before []liveCall `json:"before"`
	} `json:"kinds"`
}

type liveCall struct {
	Save map[string]string `json:"save"`
}

// liveIdentities reads a live.yaml's identities, as a live run takes them
// (kindtest's LiveConfig). No live.yaml: none.
func liveIdentities(path string) (cassette.Identities, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return cassette.Identities{}, nil
	}
	if err != nil {
		return cassette.Identities{}, err
	}
	var l liveYAML
	if err := yaml.Unmarshal(data, &l); err != nil {
		return cassette.Identities{}, fmt.Errorf("%s: %w", path, err)
	}
	id := l.Identities
	calls := append([]liveCall(nil), l.Before...)
	for _, k := range l.Kinds {
		calls = append(calls, k.Before...)
	}
	for _, c := range calls {
		for name, at := range c.Save {
			if cassette.IdentityField(at) {
				id.RedactVars = append(id.RedactVars, name)
			}
		}
	}
	return id, nil
}

// givenValues are the values to redact given in the environment variables
// named (comma-separated) and on in (nil: none), one per line.
func givenValues(envs string, getenv func(string) string, in io.Reader) ([]string, error) {
	var out []string
	add := func(text string) {
		for _, l := range strings.Split(text, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
	}
	for _, e := range strings.Split(envs, ",") {
		if e = strings.TrimSpace(e); e == "" {
			continue
		}
		v := getenv(e)
		if v == "" {
			return nil, fmt.Errorf("-redact-env: %s is empty or not set", e)
		}
		add(v)
	}
	if in != nil {
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			add(sc.Text())
		}
		if err := sc.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// redactRecords redacts every value known knows in the records file (a
// failure's reason quotes the API), and writes it (check: names it) if that
// changes it. It returns 1 if it does.
func redactRecords(path string, known *cassette.Known, check bool, w io.Writer) (int, error) {
	old, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	out := known.Redact(string(old))
	if out == string(old) {
		return 0, nil
	}
	if !json.Valid([]byte(out)) {
		return 0, fmt.Errorf("%s: redacting it would leave it invalid JSON", path)
	}
	if check {
		fmt.Fprintf(w, "%s: a reason holds a value to redact\n", path)
		return 1, nil
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil { //nolint:gosec // rewrites a committed cassette in place, keeping it readable like the rest of the checkout
		return 0, err
	}
	fmt.Fprintf(w, "rescrubbed %s\n", path)
	return 1, nil
}

// rescrub redacts every cassette in dir again (shared: all of them sharing
// what each one's redaction learns; otherwise each with its own), and
// writes (check: lists) those that change. It returns how many do, and the
// shared Known (mk makes each Known).
func rescrub(dir string, fields map[string][]string, shared bool, mk func() *cassette.Known, check bool, w io.Writer) (int, *cassette.Known, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return 0, nil, err
	}
	sort.Strings(paths)
	known := mk()
	outs := make([]*cassette.Cassette, len(paths))
	for i, p := range paths {
		c, err := cassette.Load(p)
		if err != nil {
			return 0, nil, err
		}
		if c == nil {
			continue
		}
		k := known
		if !shared {
			k = mk()
		}
		outs[i] = c.ScrubbedWith(cassette.NewScrubber(), fields[strings.TrimSuffix(filepath.Base(p), ".json")], k)
	}
	n := 0
	for i, p := range paths {
		if outs[i] == nil {
			continue
		}
		out, _ := known.Apply(outs[i]) // what a later cassette taught
		data, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return n, known, err
		}
		data = append(data, '\n')
		old, err := os.ReadFile(p)
		if err != nil {
			return n, known, err
		}
		if bytes.Equal(old, data) {
			continue
		}
		n++
		if check {
			before, _ := cassette.Load(p)
			fmt.Fprintf(w, "%s:\n", p)
			for _, d := range differences(before, out) {
				fmt.Fprintf(w, "  %s\n", d)
			}
			continue
		}
		if err := out.Save(p); err != nil {
			return n, known, err
		}
		fmt.Fprintf(w, "rescrubbed %s\n", p)
	}
	return n, known, nil
}

// differences names what changed between two versions of a cassette, and
// shows each as it would be: never as it is, which holds what is redacted
// (the output is a terminal's, or a pull request's).
func differences(a, b *cassette.Cassette) []string {
	var out []string
	cmp := func(where, x, y string) {
		if x != y {
			out = append(out, fmt.Sprintf("%s\n    -> %s", where, clip(y)))
		}
	}
	cmp("target", a.Target, b.Target)
	for i := range a.Interactions {
		if i >= len(b.Interactions) {
			break
		}
		x, y := a.Interactions[i], b.Interactions[i]
		at := fmt.Sprintf("request %d (%s %s)", i+1, x.Method, clip(y.Path))
		cmp(at+" path", x.Path, y.Path)
		cmp(at+" body", x.Body, y.Body)
		cmp(at+" response", x.Response, y.Response)
		cmp(at+" link", x.Link, y.Link)
	}
	names := make([]string, 0, len(a.Vars))
	for k := range a.Vars {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		cmp("var "+k, fmt.Sprint(a.Vars[k]), fmt.Sprint(b.Vars[k]))
	}
	return out
}

func clip(s string) string {
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
