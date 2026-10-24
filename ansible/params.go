package ansible

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Parameter names and prefixes accepted in the environment configuration and the descriptor.
const (
	paramPlaybook      = "playbook"
	paramSSHKey        = "ssh_key_secret"
	paramVaultPassword = "vault_password_secret"
	paramForks         = "forks"
	paramTimeout       = "timeout"
	paramBecome        = "become"
	paramTags          = "tags"
	paramSkipTags      = "skip_tags"
	paramMarker        = "release_marker"
	paramMarkerDir     = "release_marker_dir"
	prefixVar          = "var."
	prefixSecretVar    = "secret_var."
)

var (
	// varRe are the names of the extra variables.
	varRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,127}$`)
	// tagRe are the tag names of --tags and --skip-tags.
	tagRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
)

// options are the parameters of a request, checked and typed.
type options struct {
	playbook      string
	sshKey        string // secret name
	vaultPassword string // secret name
	forks         int
	timeout       time.Duration
	become        bool
	tags          []string
	skipTags      []string
	marker        bool
	markerDir     string
	vars          map[string]string
	secretVars    map[string]string // variable -> secret name
}

// parseOptions checks every parameter: an unknown key is an error, so a typo never goes unnoticed.
func parseOptions(params map[string]string) (*options, error) {
	o := &options{playbook: "site.yml", timeout: time.Hour, markerDir: "/var/lib/manent/releases",
		vars: map[string]string{}, secretVars: map[string]string{}}
	var errs []string
	for _, k := range slices.Sorted(maps.Keys(params)) {
		v := params[k]
		switch {
		case k == paramPlaybook:
			clean := path.Clean(v)
			if v == "" || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
				errs = append(errs, fmt.Sprintf("%s %q must be a relative path inside the artifact", k, v))
			}
			o.playbook = clean
		case k == paramSSHKey:
			o.sshKey = v
		case k == paramVaultPassword:
			o.vaultPassword = v
		case k == paramForks:
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 500 {
				errs = append(errs, fmt.Sprintf("%s %q must be an integer between 1 and 500", k, v))
			}
			o.forks = n
		case k == paramTimeout:
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				errs = append(errs, fmt.Sprintf("%s %q must be a positive duration (e.g. 30m)", k, v))
			}
			o.timeout = d
		case k == paramBecome, k == paramMarker:
			b, err := strconv.ParseBool(v)
			if err != nil {
				errs = append(errs, fmt.Sprintf("%s %q must be true or false", k, v))
			}
			if k == paramBecome {
				o.become = b
			} else {
				o.marker = b
			}
		case k == paramMarkerDir:
			if !path.IsAbs(v) || path.Clean(v) != v || strings.ContainsAny(v, " '\"\\") {
				errs = append(errs, fmt.Sprintf("%s %q must be a clean absolute path", k, v))
			}
			o.markerDir = v
		case k == paramTags, k == paramSkipTags:
			var list []string
			for _, t := range strings.Split(v, ",") {
				t = strings.TrimSpace(t)
				if !tagRe.MatchString(t) {
					errs = append(errs, fmt.Sprintf("%s: invalid tag %q", k, t))
				}
				list = append(list, t)
			}
			if k == paramTags {
				o.tags = list
			} else {
				o.skipTags = list
			}
		case strings.HasPrefix(k, prefixVar):
			name := strings.TrimPrefix(k, prefixVar)
			if !varRe.MatchString(name) || strings.HasPrefix(name, "manent_") || strings.HasPrefix(name, "ansible_") {
				errs = append(errs, fmt.Sprintf("invalid variable name %q (manent_* and ansible_* are reserved)", k))
			}
			o.vars[name] = v
		case strings.HasPrefix(k, prefixSecretVar):
			name := strings.TrimPrefix(k, prefixSecretVar)
			if !varRe.MatchString(name) || v == "" || strings.HasPrefix(name, "manent_") || strings.HasPrefix(name, "ansible_") {
				errs = append(errs, fmt.Sprintf("%s: invalid variable name or empty secret name", k))
			}
			o.secretVars[name] = v
		default:
			errs = append(errs, fmt.Sprintf("unknown parameter %q", k))
		}
	}
	for name := range o.secretVars {
		if _, dup := o.vars[name]; dup {
			errs = append(errs, fmt.Sprintf("variable %q is set both by var.%s and secret_var.%s", name, name, name))
		}
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("ansible plugin: %s", strings.Join(errs, "; "))
	}
	return o, nil
}
