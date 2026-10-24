package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// kavanara-baseline ADR-0008: limen and everything it is built from must allow
// commercial use. limen has no module dependency today; these tests keep it
// that way for any licence that would not, the moment a require is added.

func TestRepositoryLicenceIsApache2(t *testing.T) {
	data, err := os.ReadFile("LICENSE")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("Apache License")) || !bytes.Contains(data, []byte("Version 2.0, January 2004")) {
		t.Fatal("LICENSE is not the Apache License 2.0, the baseline default")
	}
}

func TestClassifyLicenceText(t *testing.T) {
	cases := map[string]string{
		"Apache License\nVersion 2.0, January 2004":                                                      "permitted",
		"Permission is hereby granted, free of charge, to any person":                                    "permitted", // MIT
		"Redistribution and use in source and binary forms, with or without modification, are permitted": "permitted", // BSD
		"Permission to use, copy, modify, and/or distribute this software for any purpose":               "permitted", // ISC
		"Mozilla Public License Version 2.0":                                                             "weak-copyleft",
		"GNU LESSER GENERAL PUBLIC LICENSE\nVersion 3":                                                   "weak-copyleft",
		"GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007":                                            "strong-copyleft",
		"GNU AFFERO GENERAL PUBLIC LICENSE":                                                              "strong-copyleft",
		"Business Source License 1.1":                                                                    "not-commercial",
		"Server Side Public License":                                                                     "not-commercial",
		"Elastic License 2.0":                                                                            "not-commercial",
		"\"Commons Clause\" License Condition v1.0\nApache License":                                      "not-commercial",
		"PolyForm Noncommercial License 1.0.0":                                                           "not-commercial",
		"All rights reserved.":                                                                           "unknown",
	}
	for text, want := range cases {
		if got := classifyLicenceText(text); got != want {
			t.Errorf("%q: got %s, want %s", text, got, want)
		}
	}
}

// goModule is the part of `go list -m -json` the gate needs.
type goModule struct {
	Path    string
	Version string
	Main    bool
	Dir     string
}

func TestDependencyLicencesAllowCommercialUse(t *testing.T) {
	out, err := exec.Command("go", "list", "-m", "-json", "all").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var m goModule
		if err := dec.Decode(&m); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if m.Main {
			continue
		}
		text := licenceFileText(m.Dir)
		switch class := classifyLicenceText(text); class {
		case "permitted", "weak-copyleft":
		default:
			t.Errorf("%s@%s: licence %s; ADR-0008 forbids shipping it without a recorded exception", m.Path, m.Version, class)
		}
	}
}

// licenceFileText returns the first licence file of a module directory, or ""
// when there is none, which classifies as unknown and fails.
func licenceFileText(dir string) string {
	if dir == "" {
		return ""
	}
	for _, pattern := range []string{"LICENSE*", "LICENCE*", "COPYING*", "license*"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		for _, m := range matches {
			if data, err := os.ReadFile(m); err == nil {
				return string(data)
			}
		}
	}
	return ""
}
