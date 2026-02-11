package coder

import (
	"fmt"
	"strings"
)

const nameSeparator = "."

// ParseTemplateName splits "<org>.<template-name>" into organization and template names.
func ParseTemplateName(name string) (org, template string, err error) {
	segments, err := parseNameSegments(name, 2, "template")
	if err != nil {
		return "", "", err
	}

	return segments[0], segments[1], nil
}

// ParseWorkspaceName splits "<org>.<user>.<workspace-name>" into organization, user, and workspace names.
func ParseWorkspaceName(name string) (org, user, workspace string, err error) {
	segments, err := parseNameSegments(name, 3, "workspace")
	if err != nil {
		return "", "", "", err
	}

	return segments[0], segments[1], segments[2], nil
}

// BuildTemplateName constructs "<org>.<template-name>".
func BuildTemplateName(org, template string) string {
	assertNameSegment("organization", org)
	assertNameSegment("template", template)

	return org + nameSeparator + template
}

// BuildWorkspaceName constructs "<org>.<user>.<workspace-name>".
func BuildWorkspaceName(org, user, workspace string) string {
	assertNameSegment("organization", org)
	assertNameSegment("user", user)
	assertNameSegment("workspace", workspace)

	return org + nameSeparator + user + nameSeparator + workspace
}

func parseNameSegments(name string, expectedSegments int, objectType string) ([]string, error) {
	if name == "" {
		return nil, fmt.Errorf("invalid %s name: name must not be empty", objectType)
	}

	expectedSeparatorCount := expectedSegments - 1
	if strings.Count(name, nameSeparator) != expectedSeparatorCount {
		return nil, fmt.Errorf(
			"invalid %s name %q: expected %d separators (%q)",
			objectType,
			name,
			expectedSeparatorCount,
			nameSeparator,
		)
	}

	segments := strings.Split(name, nameSeparator)
	if len(segments) != expectedSegments {
		return nil, fmt.Errorf(
			"assertion failed: parsed %s name %q into %d segments; expected %d",
			objectType,
			name,
			len(segments),
			expectedSegments,
		)
	}

	for segmentIndex, segment := range segments {
		if segment == "" {
			return nil, fmt.Errorf(
				"invalid %s name %q: segment %d must not be empty",
				objectType,
				name,
				segmentIndex,
			)
		}
	}

	return segments, nil
}

func assertNameSegment(segmentType, value string) {
	if value == "" {
		panic(fmt.Sprintf("assertion failed: %s must not be empty", segmentType))
	}
	if strings.Contains(value, nameSeparator) {
		panic(fmt.Sprintf("assertion failed: %s must not contain %q", segmentType, nameSeparator))
	}
}
