package libsippycup

import "os"

// execEnvironment limits ambient configuration visible to call handlers.
// Explicit values override inherited ones; reserved media variables are added
// by execEndpoint afterwards and cannot be overridden through configuration.
func execEnvironment(n *Node) []string {
	names := []string{"PATH", "SystemRoot", "WINDIR", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "TZ"}
	names = append(names, n.PassEnvironment...)
	var env []string
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}

	for name, value := range n.Environment {
		env = append(env, name+"="+value)
	}

	return env
}
