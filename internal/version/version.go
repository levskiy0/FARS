package version

import (
	"strings"
	"sync"
)

const (
	// name is the service identifier reported in responses and logs.
	name = "FARS"
	// defaultVersion applies when no git metadata is available.
	defaultVersion = "dev"
)

// Version reports the build version. It is overridden via -ldflags or git metadata.
var Version = defaultVersion

var (
	mu       sync.Mutex
	resolved string
)

// Identifier returns the formatted service/version string for responses.
func Identifier() string {
	return name + "/" + currentVersion()
}

// Override substitutes the version string and clears cached values. Intended for tests.
func Override(v string) {
	mu.Lock()
	defer mu.Unlock()
	Version = v
	resolved = ""
}

func currentVersion() string {
	mu.Lock()
	defer mu.Unlock()
	if resolved != "" {
		return resolved
	}

	candidate := strings.TrimSpace(Version)
	if candidate == "" {
		candidate = defaultVersion
	}

	resolved = candidate
	return resolved
}
