package ansible

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// groupChars are the characters not allowed in an Ansible group name.
var groupChars = regexp.MustCompile(`[^A-Za-z0-9_]`)

// groupName is the group of a target label: <label>_<value>, with the characters Ansible does
// not allow in group names replaced by "_" (region=eu-1 -> region_eu_1).
func groupName(label, value string) string {
	return groupChars.ReplaceAllString(label+"_"+value, "_")
}

// inventory builds the inventory of the targets in the YAML inventory format (written as JSON):
// connection variables per host and a group per label, so playbooks can target a region or an
// availability zone. manent's waves and batches decide which targets a run receives.
func inventory(targets []plugin.Target, keyFile string, local bool) ([]byte, error) {
	hosts := map[string]map[string]any{}
	groups := map[string]map[string]any{}
	for _, t := range targets {
		vars := map[string]any{"ansible_host": t.Host}
		if t.Port > 0 {
			vars["ansible_port"] = t.Port
		}
		if t.User != "" {
			vars["ansible_user"] = t.User
		}
		if keyFile != "" {
			vars["ansible_ssh_private_key_file"] = keyFile
		}
		if local {
			vars["ansible_connection"] = "local"
		}
		hosts[t.Host] = vars
		for k, v := range t.Labels {
			g := groupName(k, v)
			if groups[g] == nil {
				groups[g] = map[string]any{"hosts": map[string]any{}}
			}
			groups[g]["hosts"].(map[string]any)[t.Host] = map[string]any{}
		}
	}
	all := map[string]any{"hosts": hosts}
	if len(groups) > 0 {
		all["children"] = groups
	}
	return json.MarshalIndent(map[string]any{"all": all}, "", "  ")
}

// stats are the totals of a host in the JSON callback.
type stats struct {
	OK          int `json:"ok"`
	Changed     int `json:"changed"`
	Failures    int `json:"failures"`
	Unreachable int `json:"unreachable"`
	Skipped     int `json:"skipped"`
	Rescued     int `json:"rescued"`
	Ignored     int `json:"ignored"`
}

// report is the output of the ansible.posix.json callback, with the parts the plugin reads.
type report struct {
	Stats             map[string]stats           `json:"stats"`
	GlobalCustomStats map[string]json.RawMessage `json:"global_custom_stats"`
}

// parseReport reads the JSON callback output; Ansible may print warnings before the document.
func parseReport(stdout []byte) (*report, error) {
	s := string(stdout)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	var r report
	if err := json.Unmarshal([]byte(s), &r); err != nil {
		return nil, err
	}
	return &r, nil
}
