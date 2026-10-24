# manent-plugin-ansible

Ansible deploy plugin for [manent](https://github.com/manent-deployer/manent), shipped as an external binary ([hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) over gRPC) built on manent's public SDK `github.com/manent-deployer/manent/pkg/plugin/external`.

The artifact is an Ansible project (playbooks, roles and collections), versioned and pinned by sha256 like any other artifact. manent builds the inventory from the targets of each run, so its waves, batches, approvals, freezes and audit apply to configuration changes too. A dry-run is check mode. Data published with `set_stats` become plugin outputs that other deploys can import.

## Where it runs

An external plugin is a child process of the worker, so Python and `ansible-core` must be on the worker's machine. manent's distroless image has neither: run the plugin on a dedicated pool of workers built from the `server-ansible` target of manent's Dockerfile (Python, `ansible-core`, `ansible.posix` and `community.general`), and give the plugin its own queue:

```yaml
plugins:
  ansible:
    external:
      command: /usr/local/bin/manent-plugin-ansible
      sha256: "<sha256 of the binary>"
    artifact_types: [ansible]
    queue: ansible                         # the deploys of this plugin go to this queue
dbos:
  plugins: [ansible]                       # only on the workers of the pool
```

The other processes (the API, the distroless workers) know the plugin by name and artifact types and never start it.

## How a deploy runs

1. The project is extracted into a directory of the run; the playbook (`site.yml` by default) may sit at the root or in the only top-level directory.
2. The inventory lists the targets of the run with their connection variables and one group per label: `region=eu-1` becomes the group `region_eu_1`, `az=eu-1a` the group `az_eu_1a`. manent's waves and `maxUnavailable` replace `serial`: each run receives only its targets.
3. `ansible-playbook` runs with argv arguments (no shell), the `ansible.posix.json` callback and an empty configuration file: neither `/etc/ansible/ansible.cfg` nor an `ansible.cfg` in the artifact is read.
4. A failed or unreachable host fails the run, with the totals of every host. A syntax error, a missing role or bad options fail it without retries.
5. With `release_marker: true` a small playbook records the version on the targets, so manent can read the release in service.

Playbooks must be idempotent: retries and recoveries can run them again.

## Process isolation

- Every run has its own working directory and `HOME`, removed at the end. The process gets only `PATH`, `TZ` and `LANG` from the worker.
- The SSH key arrives from a secret into a 0600 file of the run; variables and secret variables go into a 0600 extra-vars file; the Ansible Vault password into a 0600 file passed with `--vault-password-file`.
- Secrets are redacted from error messages and never appear in step outputs. `--diff` is never used, because it could show secret values.
- Host keys are checked against the worker's `known_hosts` (`known_hosts` in the plugin configuration); `insecure_ignore_host_key: true` is for development only.

## Configuration

Plugin configuration (`plugins.<name>` in `manent.yml`):

| Key | Description |
|---|---|
| `artifact_types` | Artifact types handled (default `[ansible]`). |
| `ansible_playbook` | The `ansible-playbook` command (default: looked up in `PATH`). |
| `known_hosts` | known_hosts file of the targets (default `~/.ssh/known_hosts` of the worker). |
| `insecure_ignore_host_key` | `true` skips host key verification (development only). |
| `collections_path` | Collections installed on the worker, searched after those of the artifact (default `/usr/share/ansible/collections`). |
| `connection` | `ssh` (default) or `local`, which runs the tasks on the worker (tests). |

Environment configuration:

```yaml
plugin: ansible
targets:
  - {host: web-1.prod.internal, user: deploy, labels: {region: eu-1, az: eu-1a}}
  - {host: web-2.prod.internal, user: deploy, labels: {region: eu-1, az: eu-1b}}
params:
  playbook: site.yml
  ssh_key_secret: deploy-key
  become: "true"
  var.ntp_server: ntp.example.com
  secret_var.db_password: db-password
  vault_password_secret: vault-pass
  release_marker: "true"
secrets:
  - {name: deploy-key, ref: "vault:secret/data/os/prod#ssh_key"}
  - {name: db-password, ref: "vault:secret/data/os/prod#db_password"}
  - {name: vault-pass, ref: "vault:secret/data/os/prod#vault_password"}
```

| Parameter | Description |
|---|---|
| `playbook` | Playbook to run, relative to the project (default `site.yml`). |
| `ssh_key_secret` | Secret with the private SSH key. |
| `become` | `true` adds `--become`. |
| `var.<name>` | Extra variable. `manent_*` and `ansible_*` are reserved. |
| `secret_var.<name>` | Extra variable with the value of the named secret. |
| `vault_password_secret` | Secret with the Ansible Vault password. |
| `forks` | `--forks` (1-500). |
| `tags`, `skip_tags` | Comma-separated `--tags` and `--skip-tags`. |
| `timeout` | Maximum duration of a run (default `1h`). |
| `release_marker` | `true` records the version on the targets after the playbook (`release_marker_dir`, default `/var/lib/manent/releases`, a file per release; with `become` when set). |

Every run also receives the variables `manent_deployment`, `manent_tenant`, `manent_application`, `manent_environment`, `manent_artifact`, `manent_version` and `manent_sha256`. Unknown parameters are refused.

### Outputs

Data of `ansible.builtin.set_stats` that are not per host become plugin outputs: strings as they are, lists and objects as compact JSON (a list of hosts can be exported with `format: targets`). `set_stats` aggregates the values of every host by default: use `run_once: true` (or `aggregate: false`).

```yaml
- name: export the application directory
  run_once: true
  ansible.builtin.set_stats:
    data: {app_dir: /srv/shop}
```

## Release in service and rollback

With `release_marker: true` the plugin implements manent's `ReleaseInspector`: it reads the marker of every target and reports the version, or an error if the targets disagree. Rollback is manual: `manent rollback` runs the playbook of the previously released version. An automatic rollback with a `rollback_playbook` in the artifact is planned.

## The artifact

```sh
ansible-galaxy collection install -r requirements.yml -p collections   # collections inside the project
tar -czf os-config-2.1.0.tgz site.yml roles collections group_vars
```

## Build and test

```sh
make build     # bin/manent-plugin-ansible
make test      # unit tests with a fake ansible-playbook
MANENT_TEST_ANSIBLE=$(command -v ansible-playbook) MANENT_TEST_ANSIBLE_COLLECTIONS=~/.ansible/collections go test ./...
```

## License

Apache-2.0, see [LICENSE](LICENSE).
