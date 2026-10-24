package ansible

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/pkg/plugin"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// call is one run of ansible-playbook seen by the fake runner, with the files it received
// read while they existed.
type call struct {
	dir       string
	env       []string
	args      []string
	inventory map[string]any
	vars      map[string]any
	key       string
}

func (c call) envValue(k string) string {
	for _, e := range c.env {
		if v, ok := strings.CutPrefix(e, k+"="); ok {
			return v
		}
	}
	return ""
}

// fakeRunner answers with the JSON report given for the playbook (by file name).
type fakeRunner struct {
	calls   []call
	reports map[string]string
	exit    int
	stderr  string
}

func (f *fakeRunner) Run(_ context.Context, dir string, env []string, _ string, args ...string) ([]byte, []byte, int, error) {
	c := call{dir: dir, env: env, args: args}
	for i, a := range args {
		switch {
		case a == "-i":
			raw, _ := os.ReadFile(args[i+1])
			_ = json.Unmarshal(raw, &c.inventory)
		case a == "--extra-vars" && strings.HasPrefix(args[i+1], "@"):
			raw, _ := os.ReadFile(strings.TrimPrefix(args[i+1], "@"))
			_ = json.Unmarshal(raw, &c.vars)
		}
	}
	if hosts, ok := c.inventory["all"].(map[string]any)["hosts"].(map[string]any); ok {
		for _, h := range hosts {
			if k, ok := h.(map[string]any)["ansible_ssh_private_key_file"].(string); ok {
				raw, _ := os.ReadFile(k)
				c.key = string(raw)
			}
		}
	}
	f.calls = append(f.calls, c)
	pb := filepath.Base(args[len(args)-1])
	return []byte(f.reports[pb]), []byte(f.stderr), f.exit, nil
}

type secrets map[string]string

func (s secrets) Secret(_ context.Context, name string) (string, error) {
	v, ok := s[name]
	if !ok {
		return "", errors.New("unknown secret")
	}
	return v, nil
}

func archive(t *testing.T, files map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, name := range sortedKeys(files) {
		body := files[name]
		_ = tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	p := filepath.Join(t.TempDir(), "project.tgz")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

var project = map[string]string{"os-config/site.yml": "- hosts: all\n  tasks: []\n", "os-config/roles/base/tasks/main.yml": "[]"}

func newPlugin(t *testing.T, run Runner, cfg map[string]any) *Plugin {
	t.Helper()
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["ansible_playbook"] = "/bin/sh"
	d, err := Factory(cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	p := d.(*Plugin)
	p.run = run
	return p
}

func request(t *testing.T, params map[string]string) *plugin.Request {
	t.Helper()
	return &plugin.Request{DeploymentID: "d1", Tenant: "untenanted", Application: "os", Environment: "prod",
		Artifact: plugin.Artifact{Type: "ansible", Name: "os-config", Version: "2.1.0", SHA256: "abc", LocalPath: archive(t, project)},
		Targets: []plugin.Target{
			{Host: "web-1", User: "deploy", Port: 2222, Labels: map[string]string{"region": "eu-1", "az": "eu-1a"}},
			{Host: "web-2", User: "deploy", Labels: map[string]string{"region": "eu-1", "az": "eu-1b"}},
		},
		Params: params, Secrets: secrets{"key": "-----BEGIN KEY-----", "db": "s3cr3t-db", "vault": "vault-pass"}}
}

const okReport = `[WARNING]: something
{"plays": [], "stats": {"web-1": {"ok": 5, "changed": 2, "failures": 0, "unreachable": 0, "skipped": 1},
 "web-2": {"ok": 5, "changed": 0, "failures": 0, "unreachable": 0, "skipped": 1}},
 "custom_stats": {}, "global_custom_stats": {"db_host": "db1.internal", "vms": [{"host": "10.0.0.4"}], "bad key": "x"}}`

func TestDeploy(t *testing.T) {
	t.Setenv("LEAK_ME", "worker-secret")
	run := &fakeRunner{reports: map[string]string{"site.yml": okReport}}
	p := newPlugin(t, run, map[string]any{"known_hosts": "/etc/manent/known_hosts"})
	params := map[string]string{"ssh_key_secret": "key", "var.app_port": "8080", "secret_var.db_password": "db",
		"vault_password_secret": "vault", "forks": "10", "become": "true", "tags": "base, app"}
	res, err := p.Deploy(context.Background(), request(t, params))
	if err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 1 {
		t.Fatalf("calls: %d", len(run.calls))
	}
	c := run.calls[0]
	if !strings.HasSuffix(c.dir, "/os-config") || c.args[len(c.args)-1] != "site.yml" {
		t.Errorf("project root and playbook: %s %v", c.dir, c.args)
	}
	for _, want := range [][]string{{"--forks", "10"}, {"--become"}, {"--tags", "base,app"}} {
		if i := slices.Index(c.args, want[0]); i < 0 || len(want) == 2 && c.args[i+1] != want[1] {
			t.Errorf("missing %v in %v", want, c.args)
		}
	}
	if slices.Contains(c.args, "--check") || slices.Contains(c.args, "--diff") {
		t.Errorf("a deploy runs neither check nor diff: %v", c.args)
	}
	hosts := c.inventory["all"].(map[string]any)["hosts"].(map[string]any)
	w1 := hosts["web-1"].(map[string]any)
	if w1["ansible_user"] != "deploy" || w1["ansible_port"] != float64(2222) {
		t.Errorf("inventory host: %v", w1)
	}
	groups := c.inventory["all"].(map[string]any)["children"].(map[string]any)
	if _, ok := groups["region_eu_1"].(map[string]any)["hosts"].(map[string]any)["web-2"]; !ok || groups["az_eu_1a"] == nil {
		t.Errorf("label groups: %v", groups)
	}
	if c.key != "-----BEGIN KEY-----\n" {
		t.Errorf("SSH key file: %q", c.key)
	}
	if c.vars["app_port"] != "8080" || c.vars["db_password"] != "s3cr3t-db" || c.vars["manent_version"] != "2.1.0" || c.vars["manent_deployment"] != "d1" {
		t.Errorf("extra vars: %v", c.vars)
	}
	if c.envValue("ANSIBLE_STDOUT_CALLBACK") != "ansible.posix.json" || c.envValue("ANSIBLE_HOST_KEY_CHECKING") != "true" ||
		!strings.Contains(c.envValue("ANSIBLE_SSH_ARGS"), "UserKnownHostsFile=/etc/manent/known_hosts") ||
		!strings.HasSuffix(c.envValue("ANSIBLE_CONFIG"), "ansible.cfg") || !strings.HasPrefix(c.envValue("ANSIBLE_COLLECTIONS_PATH"), c.dir+"/collections:") {
		t.Errorf("environment: %v", c.env)
	}
	if c.envValue("LEAK_ME") != "" {
		t.Error("the worker environment leaked into ansible-playbook")
	}
	if i := slices.Index(c.args, "--vault-password-file"); i < 0 {
		t.Error("vault password file missing")
	}
	if _, err := os.Stat(filepath.Dir(c.envValue("ANSIBLE_CONFIG"))); !os.IsNotExist(err) {
		t.Error("the working directory (key, variables) was not removed")
	}
	if res.Targets[0].Status != plugin.StatusDeployed || res.Targets[0].Details["changed"] != "2" || res.Targets[1].Details["ok"] != "5" {
		t.Errorf("targets: %+v", res.Targets)
	}
	if res.Outputs["db_host"] != "db1.internal" || res.Outputs["vms"] != `[{"host":"10.0.0.4"}]` || len(res.Outputs) != 2 {
		t.Errorf("outputs from set_stats: %v", res.Outputs)
	}
}

func TestDryRun(t *testing.T) {
	run := &fakeRunner{reports: map[string]string{"site.yml": okReport}}
	p := newPlugin(t, run, map[string]any{"insecure_ignore_host_key": true})
	res, err := p.DryRun(context.Background(), request(t, map[string]string{"release_marker": "true"}))
	if err != nil {
		t.Fatal(err)
	}
	c := run.calls[0]
	if !slices.Contains(c.args, "--check") || slices.Contains(c.args, "--diff") || len(run.calls) != 1 {
		t.Errorf("dry-run: check mode, no diff, no marker: %v (%d calls)", c.args, len(run.calls))
	}
	if c.envValue("ANSIBLE_HOST_KEY_CHECKING") != "false" || c.envValue("ANSIBLE_SSH_ARGS") != "" {
		t.Errorf("insecure_ignore_host_key: %v", c.env)
	}
	if res.Targets[0].Status != plugin.StatusReady || res.Outputs != nil {
		t.Errorf("dry-run result: %+v", res)
	}
}

func TestFailedHosts(t *testing.T) {
	rep := `{"plays": [], "stats": {"web-1": {"ok": 3, "failures": 1}, "web-2": {"ok": 0, "unreachable": 1}}, "global_custom_stats": {}}`
	run := &fakeRunner{reports: map[string]string{"site.yml": rep}, exit: 2}
	p := newPlugin(t, run, nil)
	res, err := p.Deploy(context.Background(), request(t, nil))
	if err == nil || !strings.Contains(err.Error(), "web-1 (failed 1, unreachable 0), web-2 (failed 0, unreachable 1)") {
		t.Fatalf("error: %v", err)
	}
	if errors.Is(err, plugin.ErrPermanent) {
		t.Error("a failed or unreachable host can be transient: no permanent error")
	}
	if res == nil || res.Targets[0].Status != plugin.StatusFailed || res.Targets[1].Status != plugin.StatusFailed {
		t.Errorf("targets: %+v", res)
	}
}

func TestSyntaxErrorIsPermanent(t *testing.T) {
	run := &fakeRunner{exit: 4, stderr: "ERROR! the role 'missing' was not found, password vault-pass"}
	p := newPlugin(t, run, nil)
	_, err := p.Deploy(context.Background(), request(t, map[string]string{"vault_password_secret": "vault"}))
	if !errors.Is(err, plugin.ErrPermanent) || !strings.Contains(err.Error(), "role 'missing'") || strings.Contains(err.Error(), "vault-pass") {
		t.Fatalf("error: %v", err)
	}
}

func TestReleaseMarker(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte(`{"version":"2.0.0","deployment":"d0"}`))
	readReport := `{"plays": [{"tasks": [{"task": {"name": "stat"}, "hosts": {}},
	 {"task": {"name": "slurp"}, "hosts": {"web-1": {"content": "` + content + `"}, "web-2": {"content": "` + content + `"}}}]}], "stats": {}}`
	run := &fakeRunner{reports: map[string]string{"site.yml": okReport, "manent-marker.yml": `{"stats": {"web-1": {"ok": 2}, "web-2": {"ok": 2}}}`,
		"manent-marker-read.yml": readReport}}
	p := newPlugin(t, run, nil)
	req := request(t, map[string]string{"release_marker": "true", "become": "true"})
	if _, err := p.Deploy(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(run.calls) != 2 {
		t.Fatalf("playbook and marker: %d calls", len(run.calls))
	}
	m := run.calls[1]
	var vars map[string]string
	for i, a := range m.args {
		if a == "--extra-vars" && !strings.HasPrefix(m.args[i+1], "@") {
			_ = json.Unmarshal([]byte(m.args[i+1]), &vars)
		}
	}
	if vars["manent_marker_path"] != "/var/lib/manent/releases/untenanted/os/os-config/prod.json" || !strings.Contains(vars["manent_marker"], `"version":"2.1.0"`) {
		t.Errorf("marker variables: %v", vars)
	}
	if !slices.Contains(m.args, "--become") {
		t.Error("the marker is written with become, like the playbook")
	}
	rel, err := p.CurrentRelease(context.Background(), req)
	if err != nil || rel == nil || rel.Version != "2.0.0" || rel.Details["targets"] != "web-1,web-2" {
		t.Fatalf("current release: %+v %v", rel, err)
	}
	// Different versions on the targets, and no marker at all.
	other := base64.StdEncoding.EncodeToString([]byte(`{"version":"1.9.0"}`))
	run.reports["manent-marker-read.yml"] = strings.Replace(readReport, `"web-2": {"content": "`+content+`"}`, `"web-2": {"content": "`+other+`"}`, 1)
	if _, err := p.CurrentRelease(context.Background(), req); err == nil || !strings.Contains(err.Error(), "different releases") {
		t.Errorf("different versions: %v", err)
	}
	run.reports["manent-marker-read.yml"] = `{"plays": [{"tasks": [{"task": {"name": "slurp"}, "hosts": {"web-1": {"skipped": true}}}]}]}`
	if rel, err := p.CurrentRelease(context.Background(), req); err != nil || rel != nil {
		t.Errorf("no marker: %+v %v", rel, err)
	}
	if _, err := p.CurrentRelease(context.Background(), request(t, nil)); err == nil {
		t.Error("without release_marker the release cannot be read")
	}
}

func TestValidate(t *testing.T) {
	p := newPlugin(t, &fakeRunner{}, nil)
	for name, tc := range map[string]struct {
		params  map[string]string
		targets []plugin.Target
		want    string
	}{
		"unknown":       {map[string]string{"playbok": "x"}, nil, `unknown parameter "playbok"`},
		"reserved var":  {map[string]string{"var.manent_version": "x"}, nil, "reserved"},
		"playbook path": {map[string]string{"playbook": "../x.yml"}, nil, "relative path"},
		"bad tag":       {map[string]string{"tags": "a b"}, nil, "invalid tag"},
		"marker dir":    {map[string]string{"release_marker_dir": "relative"}, nil, "absolute path"},
		"duplicate":     {nil, []plugin.Target{{Host: "a"}, {Host: "a"}}, "listed twice"},
		"bad host":      {nil, []plugin.Target{{Host: "a,b"}}, "invalid target host"},
	} {
		req := &plugin.Request{Params: tc.params, Targets: tc.targets}
		if req.Targets == nil {
			req.Targets = []plugin.Target{{Host: "web-1"}}
		}
		if err := p.Validate(req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := p.Validate(&plugin.Request{}); err == nil || !strings.Contains(err.Error(), "no targets") {
		t.Errorf("no targets: %v", err)
	}
}

func TestArtifactChecks(t *testing.T) {
	run := &fakeRunner{}
	p := newPlugin(t, run, nil)
	req := request(t, map[string]string{"playbook": "deploy.yml"})
	if _, err := p.Deploy(context.Background(), req); !errors.Is(err, plugin.ErrPermanent) || !strings.Contains(err.Error(), "playbook deploy.yml not found") {
		t.Fatalf("missing playbook: %v", err)
	}
	if len(run.calls) > 0 {
		t.Error("ansible-playbook ran without a playbook")
	}
}

func TestFactory(t *testing.T) {
	d, err := Factory(map[string]any{"artifact_types": []any{"config"}, "connection": "local", "queue": "ansible-pool"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != Name || !d.Supports("config") || d.Supports("ansible") || !d.(*Plugin).local {
		t.Error("configuration not applied")
	}
	for _, cfg := range []map[string]any{{"artifact_types": "x"}, {"connection": "winrm"}, {"insecure_ignore_host_key": "yes"}, {"known_hosts": ""}} {
		if _, err := Factory(cfg, nil); err == nil {
			t.Errorf("invalid configuration accepted: %v", cfg)
		}
	}
	d, _ = Factory(map[string]any{"ansible_playbook": "ansible-playbook-not-installed"}, nil)
	if _, err := d.Deploy(context.Background(), request(t, nil)); err == nil || !strings.Contains(err.Error(), "not found on the worker") {
		t.Errorf("missing ansible-playbook: %v", err)
	}
}

func TestGroupName(t *testing.T) {
	if g := groupName("region", "eu-1"); g != "region_eu_1" {
		t.Errorf("group: %s", g)
	}
}
