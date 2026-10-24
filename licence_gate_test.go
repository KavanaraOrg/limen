package main

import (
	"regexp"
	"strings"
)

// The classes of kavanara-baseline ADR-0008, matched against a licence file's
// text in lower case. The gate lives in tests because limen ships no code that
// needs it: `make test` runs it on every build.
var (
	licenceNotCommercial = regexp.MustCompile(`business source license|server side public license|elastic license|commons clause|polyform|noncommercial|non-commercial`)
	licenceWeak          = regexp.MustCompile(`lesser general public license|mozilla public license|eclipse public license|common development and distribution`)
	licenceStrong        = regexp.MustCompile(`general public license|european union public licence|open software license`)
	licencePermitted     = regexp.MustCompile(`apache license|permission is hereby granted, free of charge|redistribution and use in source and binary forms|permission to use, copy, modify, and/or distribute|this is free and unencumbered software`)
)

// classifyLicenceText returns permitted, weak-copyleft, strong-copyleft,
// not-commercial or unknown. A restriction on commercial use wins over the
// permissive text it is usually attached to (Commons Clause on Apache).
func classifyLicenceText(text string) string {
	t := strings.ToLower(text)
	switch {
	case licenceNotCommercial.MatchString(t):
		return "not-commercial"
	case licenceWeak.MatchString(t):
		return "weak-copyleft"
	case licenceStrong.MatchString(t):
		return "strong-copyleft"
	case licencePermitted.MatchString(t):
		return "permitted"
	}
	return "unknown"
}
