# manent-plugin-ansible

Ansible deploy plugin for [manent](https://github.com/manent-deployer/manent), shipped as an external binary ([hashicorp/go-plugin](https://github.com/hashicorp/go-plugin) over gRPC) built on manent's public SDK `github.com/manent-deployer/manent/pkg/plugin/external`.

> **Status:** scaffolding. The plugin builds and registers, but `Deploy` refuses every request until it is implemented.

## Planned behaviour

- Artifact of type `ansible`: a `.tgz` with playbooks, roles and collections included; `galaxy: install` optional, with allowed servers.
- Runs on a dedicated pool of workers with Python and `ansible-core` (image with `ansible.posix` and `community.general`), reached through a per-plugin queue.
- Inventory generated from the targets of the pass or batch, with one group per label; manent's waves and `maxUnavailable` replace `serial`.
- SSH key from a secret in a per-attempt directory (`umask 077`); `var.<name>` and `secret_var.<name>` in a 0600 extra-vars file; Ansible Vault password from a secret.
- `ansible-playbook` with argv arguments (no shell) and the JSON callback; a failed or unreachable host fails the pass.
- Dry-run: `--check` (without `--diff`), with ok/changed/failed counts per host.
- `set_stats` data become plugin outputs; optional `release_marker` for the live release, rollback and reconciliation.
- Automatic rollback only with a `rollback_playbook` in the artifact.

## Build

```sh
make build     # bin/manent-plugin-ansible
make test
```

## Configuration

```yaml
plugins:
  ansible:
    external:
      command: /opt/manent/plugins/manent-plugin-ansible
      sha256: "<sha256 of the binary>"
    artifact_types: [ansible]
```

manent starts the process at the first deploy, verifies its sha256 at every start and passes secrets only through `req.Secrets`, for the duration of a call. See [External plugins](https://manent-deployer.github.io/manent/plugins/esterni/).

## License

Apache-2.0, see [LICENSE](LICENSE).
