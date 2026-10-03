package builder

import (
	"fmt"
	"runtime"
	"strings"

	"github.com/cuixudong74/container-image-builder-go/internal/oci"
)

// Platform is a normalized target platform.
type Platform struct {
	OS      string
	Arch    string
	Variant string
}

// ParsePlatform parses "os/arch[/variant]" strings.
func ParsePlatform(s string) (Platform, error) {
	if s == "" {
		return CurrentPlatform(), nil
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return Platform{}, fmt.Errorf("invalid platform %q: expected os/arch[/variant]", s)
	}
	p := Platform{OS: parts[0], Arch: parts[1]}
	if len(parts) == 3 {
		p.Variant = parts[2]
	}
	if p.OS == "" || p.Arch == "" {
		return Platform{}, fmt.Errorf("invalid platform %q: os and arch must not be empty", s)
	}
	return p, nil
}

// ParsePlatforms parses a comma-separated list of platforms.
func ParsePlatforms(s string) ([]Platform, error) {
	if strings.TrimSpace(s) == "" {
		return []Platform{CurrentPlatform()}, nil
	}
	var out []Platform
	for _, part := range strings.Split(s, ",") {
		p, err := ParsePlatform(strings.TrimSpace(part))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// CurrentPlatform returns the host platform.
func CurrentPlatform() Platform {
	return Platform{OS: runtime.GOOS, Arch: runtime.GOARCH}
}

// String returns the canonical "os/arch[/variant]" form.
func (p Platform) String() string {
	s := p.OS + "/" + p.Arch
	if p.Variant != "" {
		s += "/" + p.Variant
	}
	return s
}

// OCI converts to an OCI platform descriptor.
func (p Platform) OCI() oci.Platform {
	return oci.Platform{OS: p.OS, Architecture: p.Arch, Variant: p.Variant}
}
