package provenance

import (
	"errors"
	"fmt"
	"strings"
)

const (
	kustomizeGroup = "kustomize.toolkit.fluxcd.io"
	sourceGroup    = "source.toolkit.fluxcd.io"
	kindKustomize  = "Kustomization"
)

// ErrUnknownObject is returned by Add for an object that is neither a Flux
// Kustomization nor a Flux GitRepository.
var ErrUnknownObject = errors.New("not a flux kustomization or git repository")

// Add records one decoded Kubernetes object (the shape of `kubectl get -o
// json`, or an unstructured object's content) as a Kustomization or
// GitRepository. Any other apiVersion/kind is an error: the caller hands over
// the Flux objects it read, nothing else.
func (f *Flux) Add(obj map[string]any) error {
	apiVersion := stringAt(obj, "apiVersion")
	group, _, _ := strings.Cut(apiVersion, "/")
	kind := stringAt(obj, "kind")
	name := stringAt(obj, "metadata", "name")
	namespace := stringAt(obj, "metadata", "namespace")
	if name == "" || namespace == "" {
		return fmt.Errorf("%s %s: metadata.name and metadata.namespace are required", apiVersion, kind)
	}
	switch {
	case group == kustomizeGroup && kind == kindKustomize:
		f.Kustomizations = append(f.Kustomizations, Kustomization{
			Name:      name,
			Namespace: namespace,
			SourceRef: SourceRef{
				Kind:      stringAt(obj, "spec", "sourceRef", "kind"),
				Name:      stringAt(obj, "spec", "sourceRef", "name"),
				Namespace: stringAt(obj, "spec", "sourceRef", "namespace"),
			},
			Path:               stringAt(obj, "spec", "path"),
			ServiceAccountName: stringAt(obj, "spec", "serviceAccountName"),
			Interval:           stringAt(obj, "spec", "interval"),
			Timeout:            stringAt(obj, "spec", "timeout"),
			Prune:              boolAt(obj, "spec", "prune"),
			Keys:               keysAt(obj),
		})
	case group == sourceGroup && kind == KindGitRepository:
		f.GitRepositories = append(f.GitRepositories, GitRepository{
			Name:      name,
			Namespace: namespace,
			URL:       stringAt(obj, "spec", "url"),
			Branch:    stringAt(obj, "spec", "ref", "branch"),
		})
	default:
		return fmt.Errorf("%w: %s %s %s/%s", ErrUnknownObject, apiVersion, kind, namespace, name)
	}
	return nil
}

// keysAt returns the object's spec.decryption, nil when it has none.
func keysAt(obj map[string]any) *Keys {
	provider := stringAt(obj, "spec", "decryption", "provider")
	if provider == "" {
		return nil
	}
	return &Keys{Provider: provider, SecretRef: stringAt(obj, "spec", "decryption", "secretRef", "name")}
}

// boolAt returns the bool at the nested keys, false when absent or not a bool.
func boolAt(obj map[string]any, keys ...string) bool {
	v, _ := valueAt(obj, keys...).(bool)
	return v
}

// stringAt returns the string at the nested keys, "" when absent or not a string.
func stringAt(obj map[string]any, keys ...string) string {
	s, _ := valueAt(obj, keys...).(string)
	return s
}

// valueAt returns the value at the nested keys, nil when absent.
func valueAt(obj map[string]any, keys ...string) any {
	var current any = obj
	for _, key := range keys {
		m, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = m[key]
	}
	return current
}
