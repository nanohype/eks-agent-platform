/*
Copyright 2026 stxkxs.

Licensed under the Apache License, Version 2.0 (the "License");
*/

// Package admissibility holds the corpus in testdata/cr-admissibility to
// a real API server.
//
// The corpus is the shared answer to "would the API server accept this custom
// resource?". Several readings of that question exist, each written against the
// rules its author had met, and a custom resource was only as validated as the
// reading that happened to see it. scripts/crd_admissibility.py is the one
// reading; the header on each fixture is what it must say.
//
// A header checked only against the reading that produced it is a reading
// agreeing with itself. This suite takes the same files to the authority: it
// installs the CustomResourceDefinitions the operator chart ships — the same
// files the reading resolves schemas from — and creates every fixture. A
// fixture declaring a refusal must be rejected naming that path; one declaring
// a pruning must be created with that path gone; one declaring nothing must be
// created whole. So must every example under examples/, which is the tree a
// person copies from.
//
// The CEL rules are the reason this suite exists rather than a second reading in
// Go. A rule is a program and the API server is the thing that runs it, so a
// fixture whose only defect is a CEL rule is refused here and admitted by the
// reading — and that gap is measured on every run instead of assumed.
package admissibility

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// The chart's copy of the CustomResourceDefinitions, not the generated one: it
// is what a cluster installs and what the reading resolves schemas from, so it
// is what the verdicts here are about.
var (
	repoRoot   = filepath.Join("..", "..", "..")
	crdDir     = filepath.Join(repoRoot, "charts", "operator", "crds")
	corpusDir  = filepath.Join(repoRoot, "testdata", "cr-admissibility")
	examplesIn = filepath.Join(repoRoot, "examples")

	// Every namespaced fixture and example declares this namespace, and the
	// API server will not create an object in one that does not exist.
	namespace = "eks-agent-platform"
)

var (
	testEnv   *envtest.Environment
	k8sClient client.Client
)

func TestMain(m *testing.M) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		fmt.Fprintln(os.Stderr,
			"admissibility: KUBEBUILDER_ASSETS is unset, so there is no API server to be the authority — skipping.\n"+
				"Run `make test`, which resolves the control-plane binaries via setup-envtest.")
		os.Exit(0)
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{crdDir},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		panic("envtest start: " + err.Error())
	}
	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		_ = testEnv.Stop()
		panic("client.New: " + err.Error())
	}
	if err := k8sClient.Create(context.Background(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace},
	}); err != nil {
		_ = testEnv.Stop()
		panic("create namespace: " + err.Error())
	}

	code := m.Run()
	_ = testEnv.Stop()
	os.Exit(code)
}

const (
	verdictRefused = "refused"
	verdictPruned  = "pruned"
	// The API server refuses it and the reading cannot say why, because the
	// defect is a CEL rule. To this suite it is a refusal like any other; the
	// difference is on the other side, where the reading is required to report
	// nothing about the same file. That pair is what keeps the one limit the
	// reading cannot close measured on every run.
	verdictCELRefused = "cel-refused"
)

// expectation is one `# admissibility: <verdict> <rule> <path>` line.
type expectation struct {
	verdict string
	rule    string
	path    string
}

var headerLine = regexp.MustCompile(`(?m)^#\s*admissibility:\s*(.+?)\s*$`)

// readHeaders returns what a fixture declares. A file with no header at all
// returns (nil, false): the caller fails rather than treating silence as
// "admitted", because silence is also what a file nobody finished looks like.
func readHeaders(t *testing.T, body string) ([]expectation, bool) {
	t.Helper()
	matches := headerLine.FindAllStringSubmatch(body, -1)
	if len(matches) == 0 {
		return nil, false
	}
	var out []expectation
	for _, m := range matches {
		fields := strings.Fields(m[1])
		if len(fields) == 1 && fields[0] == "admitted" {
			continue
		}
		if len(fields) != 3 || (fields[0] != verdictRefused && fields[0] != verdictPruned && fields[0] != verdictCELRefused) {
			t.Fatalf("header %q is not `admitted`, `refused <rule> <path>`, `pruned <rule> <path>` or `cel-refused <rule> <path>`", m[1])
		}
		out = append(out, expectation{verdict: fields[0], rule: fields[1], path: fields[2]})
	}
	return out, true
}

var trailingIndex = regexp.MustCompile(`\[\d+\]$`)

// mentions reports whether an API server message names a path. A message locates
// an item by index where the defect is the item's own — `spec.agents[0].image` —
// and by the list where the defect is the list's, as a duplicate entry is. Both
// spellings come from the one path in the header, so the header stays a single
// string rather than one per reader.
func mentions(message, path string) bool {
	if strings.Contains(message, path) {
		return true
	}
	if trimmed := trailingIndex.ReplaceAllString(path, ""); trimmed != path {
		return strings.Contains(message, trimmed)
	}
	return false
}

// nested walks a dotted path with optional [i] segments through a decoded object.
func nested(obj map[string]any, path string) (any, bool) {
	var cursor any = obj
	for _, segment := range strings.Split(path, ".") {
		name := segment
		var indexes []int
		for {
			loc := trailingIndex.FindStringIndex(name)
			if loc == nil {
				break
			}
			i, err := strconv.Atoi(strings.Trim(name[loc[0]:loc[1]], "[]"))
			if err != nil {
				return nil, false
			}
			indexes = append([]int{i}, indexes...)
			name = name[:loc[0]]
		}
		asMap, ok := cursor.(map[string]any)
		if !ok {
			return nil, false
		}
		cursor, ok = asMap[name]
		if !ok {
			return nil, false
		}
		for _, i := range indexes {
			asList, ok := cursor.([]any)
			if !ok || i >= len(asList) {
				return nil, false
			}
			cursor = asList[i]
		}
	}
	return cursor, true
}

func decode(t *testing.T, body string) []*unstructured.Unstructured {
	t.Helper()
	reader := utilyaml.NewYAMLReader(bufio.NewReader(strings.NewReader(body)))
	var out []*unstructured.Unstructured
	for {
		chunk, err := reader.Read()
		if err != nil {
			break
		}
		if strings.TrimSpace(stripComments(string(chunk))) == "" {
			continue
		}
		obj := &unstructured.Unstructured{}
		if err := utilyaml.Unmarshal(chunk, &obj.Object); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		out = append(out, obj)
	}
	return out
}

func stripComments(s string) string {
	var kept []string
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}

// TestCorpusMatchesTheAPIServer is the authority half of the corpus.
func TestCorpusMatchesTheAPIServer(t *testing.T) {
	ctx := context.Background()
	entries, err := filepath.Glob(filepath.Join(corpusDir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob corpus: %v", err)
	}
	if len(entries) == 0 {
		t.Fatalf("no fixture under %s, so this suite asserts nothing about any reading", corpusDir)
	}

	seen := map[string]int{}
	for _, path := range entries {
		name := filepath.Base(path)
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			expectations, hasHeader := readHeaders(t, string(raw))
			if !hasHeader {
				t.Fatalf("carries no `# admissibility:` header, so nothing states what the API server should do with it")
			}
			docs := decode(t, string(raw))
			if len(docs) != 1 {
				t.Fatalf("holds %d documents; a fixture declares the verdict on one", len(docs))
			}
			obj := docs[0]

			var refused, pruned []expectation
			for _, e := range expectations {
				switch e.verdict {
				case verdictRefused, verdictCELRefused:
					refused = append(refused, e)
				case verdictPruned:
					pruned = append(pruned, e)
				}
			}
			switch {
			case len(refused) > 0:
				seen[verdictRefused]++
				for _, e := range refused {
					if e.verdict == verdictCELRefused {
						seen[verdictCELRefused]++
						break
					}
				}
			case len(pruned) > 0:
				seen[verdictPruned]++
			default:
				seen["admitted"]++
			}

			// Create overwrites obj with what the server returns, so what the
			// fixture declared is captured first — otherwise a pruning check
			// compares the pruned object against itself and always agrees.
			sent := obj.DeepCopy()
			err = k8sClient.Create(ctx, obj)

			if len(refused) > 0 {
				if err == nil {
					t.Fatalf("declares a refusal and the API server created it — the header claims a rejection this control plane does not make")
				}
				message := err.Error()
				for _, e := range refused {
					if !mentions(message, e.path) {
						t.Errorf("declares `refused %s %s` and the API server refused it for something else.\n  message: %s",
							e.rule, e.path, message)
					}
				}
				return
			}

			if err != nil {
				t.Fatalf("declares no refusal and the API server refused it: %v", err)
			}
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, obj) })

			live := &unstructured.Unstructured{}
			live.SetGroupVersionKind(obj.GroupVersionKind())
			if err := k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
				t.Fatalf("read back: %v", err)
			}
			for _, e := range pruned {
				if _, found := nested(live.Object, e.path); found {
					t.Errorf("declares `pruned %s` and the API server kept it — the object came back carrying that path", e.path)
				}
				if _, found := nested(sent.Object, e.path); !found {
					t.Errorf("declares `pruned %s` and the fixture never set it, so nothing was there to drop", e.path)
				}
			}
		})
	}

	// A corpus of only rejections agrees with a reading that refuses everything;
	// a corpus of only admissions agrees with one that admits everything; and
	// without a pruned case nothing distinguishes dropped from rejected. Without
	// a cel-refused case the gap between what the reading holds and what this
	// control plane holds is described rather than watched.
	for _, verdict := range []string{verdictRefused, verdictPruned, verdictCELRefused, "admitted"} {
		if seen[verdict] == 0 {
			t.Errorf("no fixture is %q, so this suite cannot tell a reading that always answers %q from a correct one", verdict, verdict)
		}
	}
}

// TestExamplesAreAdmitted plants the tree a person copies from.
func TestExamplesAreAdmitted(t *testing.T) {
	ctx := context.Background()
	var files []string
	err := filepath.Walk(examplesIn, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".yaml") {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk examples: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no custom resource under %s, so the tree a reader copies from is unread again", examplesIn)
	}

	planted := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, obj := range decode(t, string(raw)) {
			rel, _ := filepath.Rel(repoRoot, path)
			name := fmt.Sprintf("%s/%s/%s", rel, obj.GetKind(), obj.GetName())
			if err := k8sClient.Create(ctx, obj.DeepCopy()); err != nil && !apierrors.IsAlreadyExists(err) {
				t.Errorf("%s is refused by the API server: %v", name, err)
				continue
			}
			planted++
			cleanup := obj.DeepCopy()
			t.Cleanup(func() { _ = k8sClient.Delete(ctx, cleanup) })
		}
	}
	if planted == 0 {
		t.Fatalf("no example was created, so this reports success having admitted nothing")
	}
	t.Logf("%d example custom resource(s) admitted by a real API server", planted)
}
