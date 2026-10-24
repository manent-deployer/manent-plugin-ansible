// Command manent-plugin-ansible is the Ansible plugin of manent, served as an external process
// through the go-plugin SDK of manent. Configuration:
//
//	plugins:
//	  ansible:
//	    external:
//	      command: /usr/local/bin/manent-plugin-ansible
//	      sha256: "<sha256 of the binary>"
//	    artifact_types: [ansible]
//	    queue: ansible   # served by the workers with dbos.plugins: [ansible]
package main

import (
	"github.com/manent-deployer/manent/pkg/plugin/external"

	"github.com/manent-deployer/manent-plugin-ansible/ansible"
)

func main() {
	external.Serve(ansible.Factory)
}
