// Package ansible implements the Ansible deploy plugin of manent: the artifact is an Ansible
// project (playbooks, roles and collections), the inventory is built from the targets each run
// receives, and a deploy runs ansible-playbook with the JSON callback. A dry-run is check mode.
package ansible

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// Name is the plugin name and the artifact type it handles by default.
const Name = "ansible"

// maxErrorText limits the end of stderr reported in an error.
const maxErrorText = 2000

// outputKeyRe are the output keys manent accepts.
var outputKeyRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,63}$`)

// Exit codes of ansible-playbook that a retry cannot fix: bad options or a parser error.
var permanentExit = []int{3, 5}

// Plugin is the Ansible deployer.
type Plugin struct {
	logger      *slog.Logger
	types       []string
	playbookBin string
	knownHosts  string
	insecure    bool
	collections string
	local       bool
	run         Runner
}

// Factory builds the plugin from its configuration (plugins.<name>):
//
//	artifact_types:           artifact types handled (default [ansible])
//	ansible_playbook:         the ansible-playbook command (default looked up in PATH)
//	known_hosts:              known_hosts file of the targets (default ~/.ssh/known_hosts of the worker)
//	insecure_ignore_host_key: true to skip host key verification (development only)
//	collections_path:         collections installed on the worker, after those of the artifact
//	                          (default /usr/share/ansible/collections)
//	connection:               ssh (default) or local, which runs the tasks on the worker (tests)
func Factory(cfg map[string]any, logger *slog.Logger) (plugin.Deployer, error) {
	if logger == nil {
		logger = slog.Default()
	}
	home, _ := os.UserHomeDir()
	p := &Plugin{logger: logger, types: []string{Name}, playbookBin: "ansible-playbook", run: execRunner{},
		knownHosts: filepath.Join(home, ".ssh", "known_hosts"), collections: "/usr/share/ansible/collections"}
	for k, v := range cfg {
		switch k {
		case "artifact_types":
			list, ok := v.([]any)
			if !ok || len(list) == 0 {
				return nil, errors.New("ansible plugin: artifact_types must be a non-empty list of strings")
			}
			p.types = p.types[:0]
			for _, t := range list {
				s, ok := t.(string)
				if !ok || s == "" {
					return nil, errors.New("ansible plugin: artifact_types must contain strings")
				}
				p.types = append(p.types, s)
			}
		case "ansible_playbook", "known_hosts", "collections_path":
			s, ok := v.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("ansible plugin: %s must be a non-empty string", k)
			}
			switch k {
			case "ansible_playbook":
				p.playbookBin = s
			case "known_hosts":
				p.knownHosts = s
			case "collections_path":
				p.collections = s
			}
		case "insecure_ignore_host_key":
			b, ok := v.(bool)
			if !ok {
				return nil, errors.New("ansible plugin: insecure_ignore_host_key must be true or false")
			}
			p.insecure = b
		case "connection":
			switch v {
			case "ssh":
			case "local":
				p.local = true
			default:
				return nil, errors.New("ansible plugin: connection must be ssh or local")
			}
		default:
			// Keys of manent's plugin configuration (external, ...) are not the plugin's.
		}
	}
	return p, nil
}

// Name implements plugin.Deployer.
func (p *Plugin) Name() string { return Name }

// Supports implements plugin.Deployer.
func (p *Plugin) Supports(artifactType string) bool { return slices.Contains(p.types, artifactType) }

// Validate implements plugin.Deployer: parameters and targets, without running anything.
func (p *Plugin) Validate(req *plugin.Request) error {
	if req == nil {
		return errors.New("ansible plugin: missing request")
	}
	if len(req.Targets) == 0 {
		return errors.New("ansible plugin: no targets: the inventory is built from them")
	}
	seen := map[string]bool{}
	for _, t := range req.Targets {
		if t.Host == "" || strings.ContainsAny(t.Host, " \t\n,:[]") {
			return fmt.Errorf("ansible plugin: invalid target host %q", t.Host)
		}
		if seen[t.Host] {
			return fmt.Errorf("ansible plugin: target %s listed twice", t.Host)
		}
		seen[t.Host] = true
	}
	_, err := parseOptions(req.Params)
	return err
}

// Deploy implements plugin.Deployer: the playbook on the targets of the request.
func (p *Plugin) Deploy(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return p.execute(ctx, req, false)
}

// DryRun implements plugin.DryRunner: the playbook in check mode, without --diff (it could
// show secrets); the result has the ok/changed/failed counts per host.
func (p *Plugin) DryRun(ctx context.Context, req *plugin.Request) (*plugin.Result, error) {
	return p.execute(ctx, req, true)
}

// session is the working directory and the environment of one call, removed at the end.
type session struct {
	p       *Plugin
	req     *plugin.Request
	o       *options
	bin     string
	work    string
	project string
	env     []string
	secrets []string // resolved values, redacted from every error text
	args    []string // inventory, key and extra-vars arguments common to every run
}

// open prepares the session: artifact (when needed), inventory, SSH key, variables and the
// environment of the processes.
func (p *Plugin) open(ctx context.Context, req *plugin.Request, withArtifact bool) (*session, error) {
	if err := p.Validate(req); err != nil {
		return nil, plugin.Permanent(err)
	}
	o, _ := parseOptions(req.Params)
	bin, err := exec.LookPath(p.playbookBin)
	if err != nil {
		return nil, fmt.Errorf("ansible plugin: %q not found on the worker (does this worker serve the plugin's queue?): %w", p.playbookBin, err)
	}
	work, err := os.MkdirTemp(shortTempRoot(), "manent-ansible-")
	if err != nil {
		return nil, err
	}
	s := &session{p: p, req: req, o: o, bin: bin, work: work}
	if err := s.prepare(ctx, withArtifact); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

func (s *session) close() { _ = os.RemoveAll(s.work) }

func (s *session) prepare(ctx context.Context, withArtifact bool) error {
	home, tmp, cp := filepath.Join(s.work, "home"), filepath.Join(s.work, "tmp"), filepath.Join(s.work, "cp")
	for _, d := range []string{home, tmp, cp} {
		if err := os.Mkdir(d, 0o700); err != nil {
			return err
		}
	}
	s.project = s.work
	if withArtifact {
		src := filepath.Join(s.work, "src")
		if err := os.Mkdir(src, 0o700); err != nil {
			return err
		}
		if err := extract(s.req.Artifact.LocalPath, src); err != nil {
			return plugin.Permanent(err)
		}
		root, err := projectRoot(src, s.o.playbook)
		if err != nil {
			return plugin.Permanent(err)
		}
		s.project = root
	}
	secret := func(name string) (string, error) {
		if s.req.Secrets == nil {
			return "", fmt.Errorf("ansible plugin: secret %q requested but no secret resolver", name)
		}
		v, err := s.req.Secrets.Secret(ctx, name)
		if err != nil {
			return "", fmt.Errorf("ansible plugin: secret %q: %w", name, err)
		}
		if v != "" {
			s.secrets = append(s.secrets, v)
		}
		return v, nil
	}
	keyFile := ""
	if s.o.sshKey != "" {
		key, err := secret(s.o.sshKey)
		if err != nil {
			return err
		}
		keyFile = filepath.Join(s.work, "id")
		if err := os.WriteFile(keyFile, []byte(strings.TrimRight(key, "\n")+"\n"), 0o600); err != nil {
			return err
		}
	}
	inv, err := inventory(s.req.Targets, keyFile, s.p.local)
	if err != nil {
		return err
	}
	invFile := filepath.Join(s.work, "inventory.json")
	if err := os.WriteFile(invFile, inv, 0o600); err != nil {
		return err
	}
	vars := map[string]any{
		"manent_deployment": s.req.DeploymentID, "manent_tenant": s.req.Tenant, "manent_application": s.req.Application,
		"manent_environment": s.req.Environment, "manent_artifact": s.req.Artifact.Name, "manent_version": s.req.Artifact.Version,
		"manent_sha256": s.req.Artifact.SHA256,
	}
	for k, v := range s.o.vars {
		vars[k] = v
	}
	for k, name := range s.o.secretVars {
		v, err := secret(name)
		if err != nil {
			return err
		}
		vars[k] = v
	}
	raw, err := json.Marshal(vars)
	if err != nil {
		return err
	}
	varsFile := filepath.Join(s.work, "extra-vars.json")
	if err := os.WriteFile(varsFile, raw, 0o600); err != nil {
		return err
	}
	s.args = []string{"-i", invFile, "--extra-vars", "@" + varsFile}
	if s.o.vaultPassword != "" {
		pass, err := secret(s.o.vaultPassword)
		if err != nil {
			return err
		}
		f := filepath.Join(s.work, "vault-pass")
		if err := os.WriteFile(f, []byte(pass), 0o600); err != nil {
			return err
		}
		s.args = append(s.args, "--vault-password-file", f)
	}
	// An empty configuration file: neither /etc/ansible/ansible.cfg nor an ansible.cfg of the
	// artifact (which could point to scripts) is read.
	cfgFile := filepath.Join(s.work, "ansible.cfg")
	if err := os.WriteFile(cfgFile, []byte("[defaults]\n"), 0o600); err != nil {
		return err
	}
	collections := filepath.Join(s.project, "collections")
	if s.p.collections != "" {
		collections += ":" + s.p.collections
	}
	s.env = []string{"HOME=" + home, "TMPDIR=" + tmp, "ANSIBLE_CONFIG=" + cfgFile, "ANSIBLE_LOCAL_TEMP=" + tmp,
		"ANSIBLE_STDOUT_CALLBACK=ansible.posix.json", "ANSIBLE_SHOW_CUSTOM_STATS=True", "ANSIBLE_RETRY_FILES_ENABLED=False",
		"ANSIBLE_NOCOLOR=1", "ANSIBLE_FORCE_COLOR=0", "ANSIBLE_PYTHON_INTERPRETER=auto_silent", "ANSIBLE_SSH_CONTROL_PATH_DIR=" + cp,
		"ANSIBLE_COLLECTIONS_PATH=" + collections, "ANSIBLE_ROLES_PATH=" + filepath.Join(s.project, "roles"),
		"ANSIBLE_HOST_KEY_CHECKING=" + strconv.FormatBool(!s.p.insecure)}
	if !s.p.insecure && s.p.knownHosts != "" {
		s.env = append(s.env, "ANSIBLE_SSH_ARGS=-o UserKnownHostsFile="+s.p.knownHosts+" -o ControlMaster=auto -o ControlPersist=60s")
	}
	for _, k := range []string{"PATH", "TZ", "LANG"} {
		if v, ok := os.LookupEnv(k); ok {
			s.env = append(s.env, k+"="+v)
		}
	}
	return nil
}

// playbook runs ansible-playbook and returns the parsed JSON report; failed or unreachable
// hosts are an error that carries the report too.
func (s *session) playbook(ctx context.Context, playbook string, extra ...string) (*report, error) {
	args := append(slices.Clone(s.args), extra...)
	if s.o.forks > 0 {
		args = append(args, "--forks", strconv.Itoa(s.o.forks))
	}
	if s.o.become {
		args = append(args, "--become")
	}
	args = append(args, playbook)
	ctx, cancel := context.WithTimeout(ctx, s.o.timeout)
	defer cancel()
	stdout, stderr, exit, err := s.p.run.Run(ctx, s.project, s.env, s.bin, args...)
	if err != nil {
		return nil, fmt.Errorf("ansible plugin: ansible-playbook: %w", err)
	}
	rep, perr := parseReport(stdout)
	if perr != nil || exit != 0 && len(rep.Stats) == 0 {
		// No report: Ansible stopped before running (options, syntax, a missing role or file).
		err := fmt.Errorf("ansible plugin: ansible-playbook failed (exit %d): %s", exit, s.redact(tail(stderr, stdout)))
		if slices.Contains(permanentExit, exit) || bytesContain(stderr, "ERROR!") {
			return nil, plugin.Permanent(err)
		}
		return nil, err
	}
	var bad []string
	for _, h := range sortedKeys(rep.Stats) {
		st := rep.Stats[h]
		if st.Failures > 0 || st.Unreachable > 0 {
			bad = append(bad, fmt.Sprintf("%s (failed %d, unreachable %d)", h, st.Failures, st.Unreachable))
		}
	}
	if len(bad) > 0 || exit != 0 {
		return rep, fmt.Errorf("ansible plugin: playbook %s failed on %s (exit %d)", playbook, strings.Join(bad, ", "), exit)
	}
	return rep, nil
}

func (s *session) redact(text string) string {
	for _, v := range s.secrets {
		text = strings.ReplaceAll(text, v, "***")
	}
	return text
}

func (p *Plugin) execute(ctx context.Context, req *plugin.Request, dry bool) (*plugin.Result, error) {
	s, err := p.open(ctx, req, true)
	if err != nil {
		return nil, err
	}
	defer s.close()
	var extra []string
	if dry {
		extra = append(extra, "--check")
	}
	if len(s.o.tags) > 0 {
		extra = append(extra, "--tags", strings.Join(s.o.tags, ","))
	}
	if len(s.o.skipTags) > 0 {
		extra = append(extra, "--skip-tags", strings.Join(s.o.skipTags, ","))
	}
	log := p.logger.With("deployment", req.DeploymentID, "playbook", s.o.playbook, "dry_run", dry)
	log.InfoContext(ctx, "ansible playbook started", "targets", len(req.Targets))
	rep, err := s.playbook(ctx, s.o.playbook, extra...)
	res := &plugin.Result{Plugin: Name, Targets: targetResults(req.Targets, rep, dry, err == nil)}
	if err != nil {
		log.ErrorContext(ctx, "ansible playbook failed", "error", err)
		return res, err
	}
	log.InfoContext(ctx, "ansible playbook completed")
	if dry {
		return res, nil
	}
	res.Outputs = p.outputs(ctx, rep)
	if s.o.marker {
		if err := s.writeMarker(ctx); err != nil {
			return res, err
		}
	}
	return res, nil
}

// targetResults turns the per-host totals into the result of every target.
func targetResults(targets []plugin.Target, rep *report, dry, ok bool) []plugin.TargetResult {
	var out []plugin.TargetResult
	for _, t := range targets {
		tr := plugin.TargetResult{Host: t.Host, Status: plugin.StatusDeployed, Details: map[string]string{}}
		if dry {
			tr.Status = plugin.StatusReady
		}
		if rep != nil {
			if st, found := rep.Stats[t.Host]; found {
				tr.Details["ok"], tr.Details["changed"] = strconv.Itoa(st.OK), strconv.Itoa(st.Changed)
				tr.Details["failed"], tr.Details["unreachable"] = strconv.Itoa(st.Failures), strconv.Itoa(st.Unreachable)
				tr.Details["skipped"] = strconv.Itoa(st.Skipped)
				if st.Failures > 0 || st.Unreachable > 0 {
					tr.Status = plugin.StatusFailed
				}
			} else {
				tr.Details["tasks"] = "0"
			}
		}
		if !ok && tr.Status != plugin.StatusFailed && rep == nil {
			tr.Status = plugin.StatusFailed
		}
		out = append(out, tr)
	}
	return out
}

// outputs are the data of set_stats (not per host): strings as they are, other values as
// compact JSON, so a list of hosts can be exported as targets.
func (p *Plugin) outputs(ctx context.Context, rep *report) map[string]string {
	out := map[string]string{}
	size := 0
	for _, k := range sortedKeys(rep.GlobalCustomStats) {
		raw := rep.GlobalCustomStats[k]
		var v string
		if err := json.Unmarshal(raw, &v); err != nil {
			var val any
			if err := json.Unmarshal(raw, &val); err != nil {
				continue
			}
			b, _ := json.Marshal(val)
			v = string(b)
		}
		size += len(k) + len(v)
		if !outputKeyRe.MatchString(k) || len(out) >= plugin.MaxOutputs || size > plugin.MaxOutputBytes {
			p.logger.WarnContext(ctx, "ansible set_stats skipped: invalid key or over manent's limits", "key", k)
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// markerPath is where the release marker of the request lives on the targets.
func (s *session) markerPath() string {
	return filepath.Join(s.o.markerDir, s.req.Tenant, s.req.Application, s.req.Artifact.Name, s.req.Environment+".json")
}

// marker is the content of the release marker.
type marker struct {
	Version    string `json:"version"`
	Deployment string `json:"deployment"`
	SHA256     string `json:"sha256,omitempty"`
}

const markerWrite = `- hosts: all
  gather_facts: false
  tasks:
    - name: manent release marker directory
      ansible.builtin.file: {path: "{{ manent_marker_path | dirname }}", state: directory, mode: "0755"}
    - name: manent release marker
      ansible.builtin.copy: {dest: "{{ manent_marker_path }}", content: "{{ manent_marker }}", mode: "0644"}
`

const markerRead = `- hosts: all
  gather_facts: false
  tasks:
    - name: stat
      ansible.builtin.stat: {path: "{{ manent_marker_path }}"}
      register: manent_stat
    - name: slurp
      ansible.builtin.slurp: {src: "{{ manent_marker_path }}"}
      when: manent_stat.stat.exists
`

// writeMarker records the released version on the targets, after the playbook.
func (s *session) writeMarker(ctx context.Context) error {
	m, _ := json.Marshal(marker{Version: s.req.Artifact.Version, Deployment: s.req.DeploymentID, SHA256: s.req.Artifact.SHA256})
	pb := filepath.Join(s.work, "manent-marker.yml")
	if err := os.WriteFile(pb, []byte(markerWrite), 0o600); err != nil {
		return err
	}
	vars, _ := json.Marshal(map[string]string{"manent_marker_path": s.markerPath(), "manent_marker": string(m)})
	if _, err := s.playbook(ctx, pb, "--extra-vars", string(vars)); err != nil {
		return fmt.Errorf("ansible plugin: release marker not written: %w", err)
	}
	return nil
}

// CurrentRelease implements plugin.ReleaseInspector with release_marker: the version recorded
// on the targets by the last deploy, or nil if no target has one.
func (p *Plugin) CurrentRelease(ctx context.Context, req *plugin.Request) (*plugin.Release, error) {
	o, err := parseOptions(req.Params)
	if err != nil {
		return nil, err
	}
	if !o.marker {
		return nil, errors.New("ansible plugin: the release in service is read only with release_marker: true")
	}
	s, err := p.open(ctx, req, false)
	if err != nil {
		return nil, err
	}
	defer s.close()
	pb := filepath.Join(s.work, "manent-marker-read.yml")
	if err := os.WriteFile(pb, []byte(markerRead), 0o600); err != nil {
		return nil, err
	}
	vars, _ := json.Marshal(map[string]string{"manent_marker_path": s.markerPath()})
	stdout, stderr, exit, err := p.run.Run(ctx, s.project, s.env, s.bin, append(slices.Clone(s.args), "--extra-vars", string(vars), pb)...)
	if err != nil || exit != 0 {
		return nil, fmt.Errorf("ansible plugin: reading the release marker failed (exit %d): %v %s", exit, err, s.redact(tail(stderr, stdout)))
	}
	found, err := markerVersions(stdout)
	if err != nil {
		return nil, fmt.Errorf("ansible plugin: reading the release marker: %w", err)
	}
	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		for v, hosts := range found {
			return &plugin.Release{Version: v, Details: map[string]string{"source": "release marker", "targets": strings.Join(hosts, ",")}}, nil
		}
	}
	var desc []string
	for _, v := range sortedKeys(found) {
		desc = append(desc, v+" on "+strings.Join(found[v], ","))
	}
	return nil, fmt.Errorf("ansible plugin: different releases on the targets: %s", strings.Join(desc, "; "))
}

// markerVersions reads the slurp results of the marker playbook: version -> hosts.
func markerVersions(stdout []byte) (map[string][]string, error) {
	s := string(stdout)
	if i := strings.Index(s, "{"); i > 0 {
		s = s[i:]
	}
	var out struct {
		Plays []struct {
			Tasks []struct {
				Task  struct{ Name string } `json:"task"`
				Hosts map[string]struct {
					Content string `json:"content"`
					Skipped bool   `json:"skipped"`
				} `json:"hosts"`
			} `json:"tasks"`
		} `json:"plays"`
	}
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, err
	}
	found := map[string][]string{}
	for _, play := range out.Plays {
		for _, t := range play.Tasks {
			if t.Task.Name != "slurp" {
				continue
			}
			for _, h := range sortedKeys(t.Hosts) {
				r := t.Hosts[h]
				if r.Skipped || r.Content == "" {
					continue
				}
				raw, err := base64.StdEncoding.DecodeString(r.Content)
				if err != nil {
					return nil, err
				}
				var m marker
				if err := json.Unmarshal(raw, &m); err != nil || m.Version == "" {
					return nil, fmt.Errorf("invalid marker on %s", h)
				}
				found[m.Version] = append(found[m.Version], h)
			}
		}
	}
	return found, nil
}

// shortTempRoot is the directory of the sessions: Ansible's local RPC server and the SSH
// ControlPath are Unix sockets under it, and socket paths are limited to about 100 bytes, so a
// long TMPDIR (macOS) is replaced by /tmp.
func shortTempRoot() string {
	dir := os.TempDir()
	if len(dir) > 40 {
		if fi, err := os.Stat("/tmp"); err == nil && fi.IsDir() {
			return "/tmp"
		}
	}
	return dir
}

func tail(stderr, stdout []byte) string {
	text := strings.TrimSpace(string(stderr))
	if text == "" {
		text = strings.TrimSpace(string(stdout))
	}
	if len(text) > maxErrorText {
		text = "…" + text[len(text)-maxErrorText:]
	}
	return text
}

func bytesContain(b []byte, s string) bool { return strings.Contains(string(b), s) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
