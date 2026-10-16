package policy

import (
	"bytes"
	"fmt"
	"path"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is a parsed raw access policy document (access.yaml) before selectors
// are resolved against a partition catalog.
type File struct {
	Version     int
	Imports     []string
	Defaults    map[string]string
	Selectors   map[string]Selector
	Groups      map[string]Subject
	Users       map[string]Subject
	Clients     map[string]Subject
	AWSBindings []AWSBinding
}

// Selector matches partitions by label equality (all labels must match).
type Selector struct {
	Labels map[string]string
}

// Subject is one ACL entry: scopes (partition names, selector refs or globs),
// an optional operate role, and wholesale deny scopes.
type Subject struct {
	Partitions []string
	Logs       []string
	Traces     []string
	MonoFS     []string
	Role       string
	Deny       []string
}

type rawFile struct {
	Version     int                    `yaml:"version"`
	Imports     []string               `yaml:"imports"`
	Defaults    map[string]string      `yaml:"defaults"`
	Selectors   map[string]rawSelector `yaml:"selectors"`
	Groups      map[string]rawSubject  `yaml:"groups"`
	Users       map[string]rawSubject  `yaml:"users"`
	Clients     map[string]rawSubject  `yaml:"clients"`
	AWSBindings []AWSBinding           `yaml:"aws_bindings"`
}

type rawSelector struct {
	Labels map[string]string `yaml:"labels"`
}

type rawSubject struct {
	Partitions []string `yaml:"partitions"`
	Logs       []string `yaml:"logs"`
	Traces     []string `yaml:"traces"`
	MonoFS     []string `yaml:"monofs"`
	Role       string   `yaml:"role"`
	Deny       []string `yaml:"deny"`
}

// ParseFile parses a single access policy document without resolving imports.
// Unknown fields are rejected so typos surface at lint time.
func ParseFile(data []byte) (*File, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var doc rawFile
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("policy: parse: %w", err)
	}
	if doc.Version != 1 {
		return nil, fmt.Errorf("policy: unsupported version %d (expected 1)", doc.Version)
	}
	for scope := range doc.Defaults {
		if err := validateScope(scope); err != nil {
			return nil, err
		}
	}
	file := &File{
		Version:     doc.Version,
		Imports:     doc.Imports,
		Defaults:    doc.Defaults,
		AWSBindings: doc.AWSBindings,
	}
	if len(doc.Selectors) > 0 {
		file.Selectors = make(map[string]Selector, len(doc.Selectors))
		for name, sel := range doc.Selectors {
			if len(sel.Labels) == 0 {
				return nil, fmt.Errorf("policy: selector %q has no labels", name)
			}
			file.Selectors[name] = Selector{Labels: sel.Labels}
		}
	}
	file.Groups = convertSubjects(doc.Groups)
	file.Users = convertSubjects(doc.Users)
	file.Clients = convertSubjects(doc.Clients)
	return file, nil
}

func convertSubjects(in map[string]rawSubject) map[string]Subject {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]Subject, len(in))
	for name, sub := range in {
		out[name] = Subject{
			Partitions: sub.Partitions,
			Logs:       sub.Logs,
			Traces:     sub.Traces,
			MonoFS:     sub.MonoFS,
			Role:       sub.Role,
			Deny:       sub.Deny,
		}
	}
	return out
}

// ReadFunc resolves an import path relative to the importing file's directory
// and returns its contents.
type ReadFunc func(rel string) ([]byte, error)

// LoadFile parses a document and resolves its imports (depth-first, child wins
// on conflicts) using read for import lookups. basePath is the file's own path
// (used for relative imports and cycle detection).
func LoadFile(data []byte, basePath string, read ReadFunc) (*File, []string, error) {
	return loadFile(data, basePath, read, map[string]bool{}, 0)
}

func loadFile(data []byte, basePath string, read ReadFunc, seen map[string]bool, depth int) (*File, []string, error) {
	if depth > 10 {
		return nil, nil, fmt.Errorf("policy: import depth exceeded at %q", basePath)
	}
	if seen[basePath] {
		return nil, nil, fmt.Errorf("policy: import cycle at %q", basePath)
	}
	seen[basePath] = true
	defer delete(seen, basePath)

	file, err := ParseFile(data)
	if err != nil {
		return nil, nil, err
	}
	if len(file.Imports) == 0 {
		return file, nil, nil
	}
	if read == nil {
		return nil, nil, fmt.Errorf("policy: %q has imports but no import reader is available", basePath)
	}

	var warnings []string
	merged := &File{Version: file.Version}
	dir := path.Dir(basePath)
	for _, imp := range file.Imports {
		impPath := path.Clean(path.Join(dir, imp))
		impData, err := read(imp)
		if err != nil {
			return nil, nil, fmt.Errorf("policy: import %q: %w", imp, err)
		}
		base, warns, err := loadFile(impData, impPath, read, seen, depth+1)
		if err != nil {
			return nil, nil, err
		}
		warnings = append(warnings, warns...)
		merged = mergeFiles(merged, base)
	}
	merged = mergeFiles(merged, file)
	return merged, warnings, nil
}

// mergeFiles merges child on top of base. Subjects and selectors replace per
// key; defaults merge per scope; AWS bindings are replaced when the child
// defines any.
func mergeFiles(base, child *File) *File {
	out := &File{
		Version:   base.Version,
		Defaults:  map[string]string{},
		Selectors: map[string]Selector{},
		Groups:    map[string]Subject{},
		Users:     map[string]Subject{},
		Clients:   map[string]Subject{},
	}
	for k, v := range base.Defaults {
		out.Defaults[k] = v
	}
	for k, v := range child.Defaults {
		out.Defaults[k] = v
	}
	for k, v := range base.Selectors {
		out.Selectors[k] = v
	}
	for k, v := range child.Selectors {
		out.Selectors[k] = v
	}
	mergeSubjects := func(dst, src map[string]Subject) map[string]Subject {
		for k, v := range src {
			dst[k] = v
		}
		return dst
	}
	out.Groups = mergeSubjects(out.Groups, base.Groups)
	out.Groups = mergeSubjects(out.Groups, child.Groups)
	out.Users = mergeSubjects(out.Users, base.Users)
	out.Users = mergeSubjects(out.Users, child.Users)
	out.Clients = mergeSubjects(out.Clients, base.Clients)
	out.Clients = mergeSubjects(out.Clients, child.Clients)
	if len(child.AWSBindings) > 0 {
		out.AWSBindings = child.AWSBindings
	} else {
		out.AWSBindings = base.AWSBindings
	}
	if len(out.Defaults) == 0 {
		out.Defaults = nil
	}
	if len(out.Selectors) == 0 {
		out.Selectors = nil
	}
	if len(out.Groups) == 0 {
		out.Groups = nil
	}
	if len(out.Users) == 0 {
		out.Users = nil
	}
	if len(out.Clients) == 0 {
		out.Clients = nil
	}
	return out
}

// validateScopes returns an error for unknown or forbidden scope names.
func validateScope(scope string) error {
	if scope == string(SignalMetrics) {
		return fmt.Errorf("policy: scope %q is always visible and cannot be configured", scope)
	}
	for _, s := range AllScopes {
		if scope == s {
			return nil
		}
	}
	return fmt.Errorf("policy: unknown scope %q (expected one of %s)", scope, strings.Join(AllScopes, ", "))
}
