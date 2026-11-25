// SPDX-License-Identifier: Apache-2.0

package rest

import "testing"

// A {+param} keeps its value's slashes, escaping each part; a {param}
// escapes them. A part that is empty, . or .. is refused: it would name
// another path.
func TestExpandKeepsSlashes(t *testing.T) {
	for _, c := range []struct {
		path, branch, want string
		fails              bool
	}{
		{"/git/refs/heads/{+branch}", "feature/x y", "/git/refs/heads/feature/x%20y", false},
		{"/git/refs/heads/{branch}", "feature/x", "/git/refs/heads/feature%2Fx", false},
		{"/git/refs/heads/{+branch}", "main", "/git/refs/heads/main", false},
		{"/git/refs/heads/{+branch}", "../../orgs/x", "", true},
		{"/git/refs/heads/{+branch}", "a//b", "", true},
		{"/git/refs/heads/{+branch}", "a/./b", "", true},
		{"/git/refs/heads/{+branch}", "", "", true},
	} {
		got, err := Expand(c.path, map[string]string{"branch": c.branch})
		if (err != nil) != c.fails || got != c.want {
			t.Errorf("Expand(%s, %q) = %q, %v; want %q, fails %v", c.path, c.branch, got, err, c.want, c.fails)
		}
	}
}
