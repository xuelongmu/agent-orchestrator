// Package pathenv centralizes the effective executable search path used by
// daemon-side resolution and spawned sessions.
package pathenv

// Effective returns PATH when the environment supplies one, otherwise the
// platform's system default. getenv is injected so callers that already
// abstract their environment remain straightforward to test.
func Effective(getenv func(string) string) string {
	if path := getenv("PATH"); path != "" {
		return path
	}
	return defaultPATH
}
