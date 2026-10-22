package tasmota

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed Tasmota firmware version string such as
// "15.6.0(release-tasmota)" or "12.3.1(safeboot)".
type Version struct {
	Major, Minor, Patch int
	// Variant is the text in parentheses: the build flavour ("release-tasmota",
	// "release-tasmota32", "tasmota-sensors") or "safeboot" while an ESP32 sits
	// in its recovery partition between the two halves of an OTA.
	Variant string
	Raw     string
}

// ParseVersion parses the StatusFWR.Version form. A leading "v" is accepted so
// GitHub release tags ("v16.0.0") parse too.
func ParseVersion(s string) (Version, error) {
	v := Version{Raw: s}
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '('); i >= 0 {
		v.Variant = strings.TrimSuffix(s[i+1:], ")")
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("version %q: want major.minor.patch", v.Raw)
	}
	nums := [3]int{}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, fmt.Errorf("version %q: %q is not a number", v.Raw, p)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// Safeboot reports whether the device is running the ESP32 safeboot partition
// (the first half of an OTA has completed and the second half has not).
func (v Version) Safeboot() bool { return strings.EqualFold(v.Variant, "safeboot") }

// Short is "15.6.0" without the variant.
func (v Version) Short() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Compare orders by major, minor, patch; the variant is ignored.
func (v Version) Compare(o Version) int {
	for _, d := range [3]int{v.Major - o.Major, v.Minor - o.Minor, v.Patch - o.Patch} {
		if d < 0 {
			return -1
		}
		if d > 0 {
			return 1
		}
	}
	return 0
}

// IsESP32 reports whether the variant names a tasmota32 build.
func (v Version) IsESP32() bool { return strings.Contains(v.Variant, "tasmota32") }
