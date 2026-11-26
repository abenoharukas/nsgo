package main

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// checkKeys refuses the keys of doc, a YAML document of the file name, that yaml.v3 would drop as it decodes doc into a t: those of a mapping whose struct has no field for them, at any depth. Each error gives the key's line, the path of its mapping and the nearest key the mapping takes when one is near, such as "nsgo.yaml:12: android has no key permisions; did you mean permissions?". Maps, interfaces, yaml.Node and types that decode themselves (yaml.Unmarshaler) take any key; aliases are followed, and a merge key (<<) takes the keys of the mapping it is in.
func checkKeys(name string, doc *yaml.Node, t reflect.Type) error {
	c := keyCheck{name: name, seen: map[keyVisit]bool{}}
	c.walk(doc, t, "")
	return errors.Join(c.errs...)
}

// keyCheck is checkKeys' walk: the file's name, the nodes it walked as each type, which an alias reaches again, and what it refused.
type keyCheck struct {
	name string
	seen map[keyVisit]bool
	errs []error
}

type keyVisit struct {
	node *yaml.Node
	typ  reflect.Type
}

var (
	yamlNodeType        = reflect.TypeFor[yaml.Node]()
	yamlUnmarshalerType = reflect.TypeFor[yaml.Unmarshaler]()
)

// walk checks n, at path, as a t. A node of a kind t does not decode from is the decoder's to refuse.
func (c *keyCheck) walk(n *yaml.Node, t reflect.Type, path string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, child := range n.Content {
			c.walk(child, t, path)
		}
		return
	case yaml.AliasNode:
		if n.Alias != nil {
			c.walk(n.Alias, t, path)
		}
		return
	}
	if t == yamlNodeType || t.Kind() == reflect.Interface || reflect.PointerTo(t).Implements(yamlUnmarshalerType) {
		return
	}
	visit := keyVisit{n, t}
	if c.seen[visit] {
		return
	}
	c.seen[visit] = true
	switch {
	case t.Kind() == reflect.Struct && n.Kind == yaml.MappingNode:
		fields, rest := yamlFields(t)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if isMergeKey(key) {
				c.merge(value, t, path)
				continue
			}
			if key.Kind != yaml.ScalarNode {
				continue
			}
			typ := rest
			if at := slices.IndexFunc(fields, func(f yamlField) bool { return f.key == key.Value }); at >= 0 {
				typ = fields[at].typ
			}
			if typ == nil {
				c.unknown(key, path, fields)
				continue
			}
			c.walk(value, typ, joinKeyPath(path, key.Value))
		}
	case t.Kind() == reflect.Map && n.Kind == yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, value := n.Content[i], n.Content[i+1]
			if isMergeKey(key) {
				c.merge(value, t, path)
				continue
			}
			c.walk(value, t.Elem(), joinKeyPath(path, key.Value))
		}
	case (t.Kind() == reflect.Slice || t.Kind() == reflect.Array) && n.Kind == yaml.SequenceNode:
		for i, item := range n.Content {
			c.walk(item, t.Elem(), fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

// merge checks what a merge key brings into the mapping at path, a t: a mapping, an alias of one, or a list of those.
func (c *keyCheck) merge(n *yaml.Node, t reflect.Type, path string) {
	if n.Kind == yaml.SequenceNode {
		for _, item := range n.Content {
			c.walk(item, t, path)
		}
		return
	}
	c.walk(n, t, path)
}

// unknown refuses key, which the mapping at path, of fields, has no field for.
func (c *keyCheck) unknown(key *yaml.Node, path string, fields []yamlField) {
	in := path
	if in == "" {
		in = c.name
	}
	msg := fmt.Sprintf("%s:%d: %s has no key %s", c.name, key.Line, in, keyName(key.Value))
	if near := nearestKey(key.Value, fields); near != "" {
		msg += "; did you mean " + near + "?"
	}
	c.errs = append(c.errs, errors.New(msg))
}

// isMergeKey reports whether key is YAML's merge key as yaml.v3 reads it: <<, plain or tagged !!merge, never quoted.
func isMergeKey(key *yaml.Node) bool {
	return key.Kind == yaml.ScalarNode && key.Value == "<<" && key.ShortTag() == "!!merge"
}

func joinKeyPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// keyName is key as an error shows it: quoted when it is empty or holds a space or a character that does not print, such as a zero-width space pasted with it.
func keyName(key string) string {
	if key == "" || strings.ContainsFunc(key, func(r rune) bool { return unicode.IsSpace(r) || !unicode.IsPrint(r) }) {
		return strconv.Quote(key)
	}
	return key
}

// nearestKey is the key of fields nearest key, ignoring case, when it is within an edit for each three of key's characters, at least one; else "". Of keys as near, it is the first.
func nearestKey(key string, fields []yamlField) string {
	typed := []rune(strings.ToLower(key))
	best, within := "", max(1, len(typed)/3)
	for _, f := range fields {
		if d := editDistance(typed, []rune(strings.ToLower(f.key))); d <= within {
			best, within = f.key, d-1
		}
	}
	return best
}

// editDistance is the optimal string alignment distance from a to b: the fewest insertions, deletions, substitutions and swaps of neighbors that turn a into b, editing no part twice. A swap counts once, so nmae is one edit from name, not Levenshtein's two.
func editDistance(a, b []rune) int {
	// The rows of the table two before, one before and at a's i-th rune.
	before, last, row := make([]int, len(b)+1), make([]int, len(b)+1), make([]int, len(b)+1)
	for j := range last {
		last[j] = j
	}
	for i := 1; i <= len(a); i++ {
		row[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			row[j] = min(last[j]+1, row[j-1]+1, last[j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				row[j] = min(row[j], before[j-2]+1)
			}
		}
		before, last, row = last, row, before
	}
	return last[len(b)]
}

// yamlField is a key that a mapping decoding into a struct takes, and the type of its value.
type yamlField struct {
	key string
	typ reflect.Type
}

// yamlFields are struct t's keys, in its fields' order, as yaml.v3 decodes a mapping into it (getStructInfo): an exported field's is its tag's name, else its own name in lower case; a field tagged "-" has none; and a struct its tag inlines gives its own keys in its place. A map its tag inlines takes every other key, whose values are rest's, nil without one.
func yamlFields(t reflect.Type) (fields []yamlField, rest reflect.Type) {
	for f := range t.Fields() {
		if !f.IsExported() && !f.Anonymous {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "" && !strings.Contains(string(f.Tag), ":") {
			tag = string(f.Tag)
		}
		if tag == "-" {
			continue
		}
		name, flags, _ := strings.Cut(tag, ",")
		if slices.Contains(strings.Split(flags, ","), "inline") {
			inlined := f.Type
			for inlined.Kind() == reflect.Pointer {
				inlined = inlined.Elem()
			}
			if inlined.Kind() == reflect.Map {
				rest = inlined.Elem()
				continue
			}
			more, moreRest := yamlFields(inlined)
			fields = append(fields, more...)
			if moreRest != nil {
				rest = moreRest
			}
			continue
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		fields = append(fields, yamlField{name, f.Type})
	}
	return fields, rest
}
