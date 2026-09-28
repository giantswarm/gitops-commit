package layout

import (
	"bytes"
	"errors"
	"strings"

	"gopkg.in/yaml.v3"
)

// errNoMapping: a kustomization.yaml that is not a mapping document cannot
// take an entry.
var errNoMapping = errors.New("not a YAML mapping")

// newKustomization is the head of a kustomization.yaml the plan creates.
const newKustomization = "apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n"

// Resources is the resources list of a kustomization.yaml; nil content, an
// empty file and a file without the list have none.
func Resources(raw []byte) ([]string, error) {
	_, m, err := mapping(raw)
	if err != nil {
		return nil, err
	}
	seq := resourcesNode(m)
	if seq == nil || seq.Tag == "!!null" {
		return nil, nil
	}
	if seq.Kind != yaml.SequenceNode {
		return nil, errors.New("resources is not a list")
	}
	var out []string
	for _, n := range seq.Content {
		if n.Kind != yaml.ScalarNode {
			return nil, errors.New("resources holds an entry that is not a string")
		}
		out = append(out, n.Value)
	}
	return out, nil
}

// WithResources is the kustomization.yaml with its resources list replaced
// by resources, every other key and comment kept — an entry that stays keeps
// its node and so its comments; a new file for nil content.
func WithResources(raw []byte, resources []string) ([]byte, error) {
	if raw == nil {
		raw = []byte(newKustomization)
	}
	doc, m, err := mapping(raw)
	if err != nil {
		return nil, err
	}
	old := resourcesNode(m)
	kept := map[string]*yaml.Node{}
	if old != nil && old.Kind == yaml.SequenceNode {
		for _, n := range old.Content {
			kept[n.Value] = n
		}
	}
	list := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, r := range resources {
		n, ok := kept[r]
		if !ok {
			n = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: r}
		}
		list.Content = append(list.Content, n)
	}
	if old == nil {
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "resources"}, list)
	} else {
		if old.Kind == yaml.SequenceNode {
			list.Style = old.Style
		}
		list.HeadComment, list.LineComment, list.FootComment = old.HeadComment, old.LineComment, old.FootComment
		*old = *list
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// mapping parses raw as one YAML document whose root is a mapping; an empty
// file (nothing, blank lines or comments alone) is an empty mapping.
func mapping(raw []byte) (*yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, err
	}
	if doc.Kind == 0 || doc.Kind == yaml.DocumentNode && len(doc.Content) == 1 && doc.Content[0].Tag == "!!null" {
		// The comments of an empty file stay at its head.
		m := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map", HeadComment: comments(raw)}
		return &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{m}}, m, nil
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, nil, errNoMapping
	}
	return &doc, doc.Content[0], nil
}

// resourcesNode is the value node of the mapping's resources key, nil when
// the key is absent.
func resourcesNode(m *yaml.Node) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == "resources" {
			return m.Content[i+1]
		}
	}
	return nil
}

// comments is the text of raw when it holds comments and blank lines only,
// else nothing.
func comments(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	for line := range strings.Lines(text) {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			return ""
		}
	}
	return text
}
