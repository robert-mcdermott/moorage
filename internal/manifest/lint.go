package manifest

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
)

var secretName = regexp.MustCompile(`(?i)(SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|PRIVATE_?KEY|CREDENTIAL)`)

// Lint returns warnings about a valid manifest: literal values that look
// like credentials, and settings this version accepts but does not act on
// yet.
func Lint(m *Manifest) []string {
	var w []string
	add := func(format string, args ...any) { w = append(w, fmt.Sprintf(format, args...)) }

	envTables := map[string]map[string]string{"env": m.Env}
	if m.Build != nil {
		envTables["build.env"] = m.Build.Env
	}
	for name, e := range m.Environments {
		envTables["environments."+name+".env"] = e.Env
	}
	for table, env := range envTables {
		for k, v := range env {
			if looksSecret(k, v) {
				add("%s.%s looks like a credential: keep secrets out of the repository and set them on the server", table, k)
			}
		}
	}

	notYet := func(what, arrives string) { add("%s is accepted but has no effect yet (%s)", what, arrives) }
	inContainers := m.App.Runtime == "dockerfile" || m.App.Runtime == "image"
	switch m.App.Runtime {
	case "bun", "deno":
		notYet("runtime "+m.App.Runtime, "a later release")
	}
	if m.Container != nil && !inContainers && m.App.Runtime != "" && m.App.Runtime != "auto" {
		add("[container] has no effect with the %s runtime: it is for the dockerfile and image runtimes", m.App.Runtime)
	}
	if m.Bun != nil || m.Deno != nil {
		notYet("[bun] and [deno]", "a later release")
	}
	if m.App.Root != "" && m.App.Root != "." {
		notYet("app.root", "a later release; deploy the app's directory instead")
	}
	if m.App.Shell == "system" {
		notYet(`app.shell = "system"`, "commands run in the embedded shell")
	}
	if len(m.Routes) > 0 {
		notYet("[[routes]]", "path routes arrive in a later release")
	}
	if m.Storage != nil && !inContainers {
		notYet("[storage]", "it applies to containers; $MOORAGE_DATA_DIR is always available")
	}
	if m.Previews != nil {
		if m.Previews.Data != "" {
			notYet("previews.data", "a preview's data directory starts empty")
		}
	}
	if len(m.Services) > 0 {
		notYet("[services]", "services arrive in a later release")
	}
	if m.Static != nil && len(m.Processes) > 0 {
		notYet("[static] beside [processes]", "serving both arrives with [[routes]]; the processes serve the app")
	}
	sort.Strings(w)
	return w
}

func looksSecret(key, value string) bool {
	if value == "" || strings.HasPrefix(value, "$") {
		return false
	}
	if secretName.MatchString(key) {
		return true
	}
	return len(value) >= 24 && !strings.ContainsAny(value, " /") && entropy(value) > 4.0
}

// entropy is the Shannon entropy of s in bits per character.
func entropy(s string) float64 {
	counts := map[rune]float64{}
	for _, r := range s {
		counts[r]++
	}
	n := float64(len([]rune(s)))
	var h float64
	for _, c := range counts {
		p := c / n
		h -= p * math.Log2(p)
	}
	return h
}
