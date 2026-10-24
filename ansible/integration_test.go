package ansible

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/manent-deployer/manent/pkg/plugin"
)

// TestRealAnsible runs the plugin against a real ansible-playbook (MANENT_TEST_ANSIBLE, an
// absolute path) with the local connection; MANENT_TEST_ANSIBLE_COLLECTIONS is where
// ansible.posix is installed, for the JSON callback.
func TestRealAnsible(t *testing.T) {
	bin := os.Getenv("MANENT_TEST_ANSIBLE")
	if bin == "" {
		t.Skip("MANENT_TEST_ANSIBLE not set")
	}
	out := t.TempDir()
	cfg := map[string]any{"ansible_playbook": bin, "connection": "local"}
	if c := os.Getenv("MANENT_TEST_ANSIBLE_COLLECTIONS"); c != "" {
		cfg["collections_path"] = c
	}
	// ansible-playbook is a Python script: its interpreter must be found through PATH.
	t.Setenv("PATH", filepath.Dir(bin)+":"+os.Getenv("PATH"))
	d, err := Factory(cfg, discard)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"site.yml": `- hosts: all
  gather_facts: false
  roles: [motd]
  tasks:
    - name: export
      run_once: true
      ansible.builtin.set_stats:
        data:
          db_host: "db-{{ manent_environment }}.internal"
          vms: [{host: "10.0.0.4"}]
`,
		"roles/motd/tasks/main.yml": `- name: motd
  ansible.builtin.copy:
    dest: "{{ out_dir }}/{{ inventory_hostname }}.motd"
    content: "{{ greeting }} {{ manent_version }} {{ token | length }} {{ group_names | sort | join(',') }}\n"
`,
	}
	req := &plugin.Request{DeploymentID: "d1", Tenant: "untenanted", Application: "os", Environment: "prod",
		Artifact: plugin.Artifact{Type: "ansible", Name: "os-config", Version: "1.0.0", LocalPath: archive(t, files)},
		Targets:  []plugin.Target{{Host: "web-1", Labels: map[string]string{"region": "eu-1"}}, {Host: "web-2"}},
		Params: map[string]string{"var.out_dir": out, "var.greeting": "hello", "secret_var.token": "db",
			"release_marker": "true", "release_marker_dir": filepath.Join(out, "markers")},
		Secrets: secrets{"db": "s3cr3t-db"}}
	ctx := context.Background()

	dry, err := d.(plugin.DryRunner).DryRun(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if dry.Targets[0].Status != plugin.StatusReady || dry.Targets[0].Details["changed"] != "1" {
		t.Errorf("dry-run: %+v", dry.Targets)
	}
	if _, err := os.Stat(filepath.Join(out, "web-1.motd")); err == nil {
		t.Fatal("check mode changed a file")
	}

	res, err := d.Deploy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	motd, _ := os.ReadFile(filepath.Join(out, "web-1.motd"))
	if strings.TrimSpace(string(motd)) != "hello 1.0.0 9 region_eu_1" {
		t.Errorf("motd: %q", motd)
	}
	if res.Outputs["db_host"] != "db-prod.internal" || res.Outputs["vms"] != `[{"host":"10.0.0.4"}]` || res.Targets[1].Details["changed"] != "1" {
		t.Errorf("result: %+v", res)
	}
	rel, err := d.(plugin.ReleaseInspector).CurrentRelease(ctx, req)
	if err != nil || rel == nil || rel.Version != "1.0.0" {
		t.Fatalf("current release: %+v %v", rel, err)
	}

	// The same version again changes nothing.
	again, err := d.Deploy(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if again.Targets[0].Details["changed"] != "0" {
		t.Errorf("second deploy: %+v", again.Targets[0])
	}
}
